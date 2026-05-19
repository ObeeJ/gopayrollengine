package services

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go-payroll-engine/internal/models"
	"net/http"
	"os"
	"time"
)

// BVNService — KYC at onboarding; the minimum viable BVN check CBN expects.
type BVNService struct {
	mockMode bool
	apiKey   string
	baseURL  string
}

// NewBVNService — reads config from env; MOCK_MODE=true returns a successful dummy verification.
func NewBVNService() *BVNService {
	return &BVNService{
		mockMode: os.Getenv("MOCK_MODE") == "true",
		apiKey:   os.Getenv("DOJAH_API_KEY"),
		baseURL:  "https://api.dojah.io",
	}
}

type dojahBVNResponse struct {
	Entity struct {
		FirstName   string `json:"first_name"`
		LastName    string `json:"last_name"`
		PhoneNumber string `json:"phone_number"`
		DateOfBirth string `json:"date_of_birth"`
	} `json:"entity"`
	Error string `json:"error"`
}

// VerifyBVN — checks via Dojah; records the response hash, not the BVN.
func (s *BVNService) VerifyBVN(orgID, employeeID, bvn string) (*models.BVNVerification, error) {
	if s.mockMode {
		// Mock returns a successful verification — safe for dev and CI.
		v := &models.BVNVerification{
			OrganizationID: orgID,
			EmployeeID:     employeeID,
			Provider:       "mock",
			Status:         "verified",
			ResponseHash:   fmt.Sprintf("%x", sha256.Sum256([]byte("mock-bvn-"+bvn))),
			VerifiedAt:     time.Now(),
		}
		models.DB.Create(v)
		return v, nil
	}

	if s.apiKey == "" {
		return nil, fmt.Errorf("DOJAH_API_KEY not set — cannot verify BVN in non-mock mode")
	}

	// Dojah BVN lookup — we send the BVN, they confirm identity details.
	reqBody, _ := json.Marshal(map[string]string{"bvn": bvn})
	req, err := http.NewRequest("POST", s.baseURL+"/api/v1/kyc/bvn", bytes.NewBuffer(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", s.apiKey)
	req.Header.Set("AppId", os.Getenv("DOJAH_APP_ID"))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("dojah unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var dojahResp dojahBVNResponse
	if err := json.NewDecoder(resp.Body).Decode(&dojahResp); err != nil {
		return nil, fmt.Errorf("dojah response decode failed: %w", err)
	}

	status := "verified"
	if dojahResp.Error != "" || resp.StatusCode != http.StatusOK {
		status = "failed"
	}

	// Hash the raw response — we prove we checked without storing the BVN itself.
	rawBytes, _ := json.Marshal(dojahResp)
	responseHash := fmt.Sprintf("%x", sha256.Sum256(rawBytes))

	v := &models.BVNVerification{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		Provider:       "dojah",
		Status:         status,
		ResponseHash:   responseHash,
		VerifiedAt:     time.Now(),
	}
	models.DB.Create(v)

	if status == "failed" {
		return v, fmt.Errorf("BVN verification failed: %s", dojahResp.Error)
	}
	return v, nil
}
