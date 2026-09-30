package workers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"go-payroll-engine/internal/models"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

// BVNHandler — processes async BVN verification tasks with Asynq retry semantics.
type BVNHandler struct{}

// NewBVNHandler — creates the BVN verification task handler.
func NewBVNHandler() *BVNHandler { return &BVNHandler{} }

// EnqueueBVNVerification — drops a BVN task on the low-priority queue; up to 5 retries on transient failure.
func EnqueueBVNVerification(orgID, employeeID, bvn string) error {
	payload, err := json.Marshal(map[string]string{
		"org_id":      orgID,
		"employee_id": employeeID,
		"bvn":         bvn,
	})
	if err != nil {
		return err
	}
	task := asynq.NewTask(TypeVerifyBVN, payload,
		asynq.Queue("low"),
		asynq.MaxRetry(5),
	)
	_, err = Client.Enqueue(task)
	return err
}

type dojahBVNResponse struct {
	Entity struct {
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
	} `json:"entity"`
	Error string `json:"error"`
}

// ProcessBVNTask — verifies BVN via Dojah (or mock); non-nil error triggers Asynq retry with backoff.
func (h *BVNHandler) ProcessBVNTask(ctx context.Context, t *asynq.Task) error {
	var payload map[string]string
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return err
	}

	orgID, employeeID, bvn := payload["org_id"], payload["employee_id"], payload["bvn"]
	log.Printf("Verifying BVN for employee %s org %s", employeeID, orgID)

	v := &models.BVNVerification{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		VerifiedAt:     time.Now(),
	}

	if os.Getenv("MOCK_MODE") == "true" {
		v.Provider = "mock"
		v.Status = "verified"
		v.ResponseHash = fmt.Sprintf("%x", sha256.Sum256([]byte("mock-bvn-"+bvn)))
		return saveBVNVerification(ctx, v)
	}

	apiKey := os.Getenv("DOJAH_API_KEY")
	if apiKey == "" {
		return fmt.Errorf("DOJAH_API_KEY not set")
	}

	reqBody, _ := json.Marshal(map[string]string{"bvn": bvn})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.dojah.io/api/v1/kyc/bvn", bytes.NewBuffer(reqBody))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", apiKey)
	req.Header.Set("AppId", os.Getenv("DOJAH_APP_ID"))
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("dojah unreachable: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var dojahResp dojahBVNResponse
	if err := json.NewDecoder(resp.Body).Decode(&dojahResp); err != nil {
		return fmt.Errorf("dojah response decode failed: %w", err)
	}

	rawBytes, _ := json.Marshal(dojahResp)
	v.Provider = "dojah"
	v.ResponseHash = fmt.Sprintf("%x", sha256.Sum256(rawBytes))
	v.Status = "verified"
	if dojahResp.Error != "" || resp.StatusCode != http.StatusOK {
		v.Status = "failed"
	}

	if err := saveBVNVerification(ctx, v); err != nil {
		return err
	}
	if v.Status == "failed" {
		// A definitive answer from Dojah, not a transient fault: retrying
		// would only re-bill the same lookup and write another failed row.
		return fmt.Errorf("BVN verification failed: %s: %w", dojahResp.Error, asynq.SkipRetry)
	}
	return nil
}

// saveBVNVerification writes the outcome inside the employee's org scope.
// bvn_verifications has forced row-level security; an unscoped insert is
// rejected by its WITH CHECK policy under the production database role, so
// every result was silently lost there while tests (run as a superuser,
// which bypasses RLS) passed. See TestProcessBVNTask_PersistsUnderProductionRole.
func saveBVNVerification(ctx context.Context, v *models.BVNVerification) error {
	return models.WithOrgScope(ctx, v.OrganizationID, func(tx *gorm.DB) error {
		return tx.Create(v).Error
	})
}
