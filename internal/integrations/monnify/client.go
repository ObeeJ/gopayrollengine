package monnify

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"go-payroll-engine/internal/observability"
	"go-payroll-engine/pkg/money"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"time"
)

// httpTimeout — applied to every outbound Monnify call so slow APIs can't hang the worker.
const httpTimeout = 30 * time.Second

// walletNumberRe restricts wallet numbers to digits only, blocking URL injection.
var walletNumberRe = regexp.MustCompile(`^\d+$`)

type Config struct {
	APIKey    string
	SecretKey string
	BaseURL   string
}

type Client struct {
	Config      Config
	AccessToken string
	TokenExpiry time.Time
	MockMode    bool
	httpClient  *http.Client
}

// NewClient — builds the Monnify client from env; MOCK_MODE=true short-circuits real calls.
func NewClient() *Client {
	return &Client{
		Config: Config{
			APIKey:    os.Getenv("MONNIFY_API_KEY"),
			SecretKey: os.Getenv("MONNIFY_SECRET_KEY"),
			BaseURL:   os.Getenv("MONNIFY_BASE_URL"),
		},
		MockMode:   os.Getenv("MOCK_MODE") == "true",
		httpClient: &http.Client{Timeout: httpTimeout}, // shared client reuses TCP connections
	}
}

type AuthResponse struct {
	RequestSuccessful bool `json:"requestSuccessful"`
	ResponseBody      struct {
		AccessToken string `json:"accessToken"`
		ExpiresIn   int    `json:"expiresIn"`
	} `json:"responseBody"`
}

// Authenticate — fetches and caches the Monnify Bearer token until expiry.
func (c *Client) Authenticate() error {
	if c.MockMode {
		// In mock mode, use a placeholder token — no real auth needed.
		c.AccessToken = "mock_token"
		c.TokenExpiry = time.Now().Add(1 * time.Hour)
		return nil
	}
	// Return early if the cached token is still valid.
	if c.AccessToken != "" && time.Now().Before(c.TokenExpiry) {
		return nil
	}

	authStr := base64.StdEncoding.EncodeToString([]byte(c.Config.APIKey + ":" + c.Config.SecretKey))
	req, _ := http.NewRequest("POST", c.Config.BaseURL+"/api/v1/auth/login", nil)
	req.Header.Set("Authorization", "Basic "+authStr)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var authResp AuthResponse
	json.NewDecoder(resp.Body).Decode(&authResp)

	if !authResp.RequestSuccessful {
		return fmt.Errorf("monnify authentication failed")
	}

	c.AccessToken = authResp.ResponseBody.AccessToken
	c.TokenExpiry = time.Now().Add(time.Duration(authResp.ResponseBody.ExpiresIn) * time.Second)
	return nil
}

type BulkTransferRequest struct {
	Title                     string           `json:"title"`
	BatchReference            string           `json:"batchReference"`
	SourceWalletAccountNumber string           `json:"sourceWalletAccountNumber"`
	TransactionList           []TransferDetail `json:"transactionList"`
}

// TransferDetail — wire format Monnify wants: float Naira; we convert once at the boundary.
type TransferDetail struct {
	Amount        float64 `json:"amount"`
	AccountNumber string  `json:"destinationAccountNumber"`
	BankCode      string  `json:"destinationBankCode"`
	Narration     string  `json:"narration"`
	Reference     string  `json:"reference"`
	CurrencyCode  string  `json:"currencyCode"`
}

type BulkTransferResponse struct {
	RequestSuccessful bool   `json:"requestSuccessful"`
	ResponseMessage   string `json:"responseMessage"`
	ResponseBody      struct {
		BatchReference string `json:"batchReference"`
		Status         string `json:"status"`
	} `json:"responseBody"`
}

// InitiateBulkTransfer — submits a batch of disbursements; outcomes arrive later via webhook.
func (c *Client) InitiateBulkTransfer(payload BulkTransferRequest) (*BulkTransferResponse, error) {
	start := time.Now()
	defer func() {
		observability.MonnifyCallDuration.WithLabelValues("bulk_transfer").Observe(time.Since(start).Seconds())
	}()

	if c.MockMode {
		observability.MonnifyCallsTotal.WithLabelValues("bulk_transfer", "true").Inc()
		return &BulkTransferResponse{
			RequestSuccessful: true,
			ResponseMessage:   "Mock Transfer Successful",
			ResponseBody: struct {
				BatchReference string `json:"batchReference"`
				Status         string `json:"status"`
			}{BatchReference: payload.BatchReference, Status: "SUCCESSFUL"},
		}, nil
	}
	if err := c.Authenticate(); err != nil {
		observability.MonnifyCallsTotal.WithLabelValues("bulk_transfer", "false").Inc()
		return nil, err
	}

	body, _ := json.Marshal(payload)
	req, _ := http.NewRequest("POST", c.Config.BaseURL+"/api/v1/disbursements/batch", bytes.NewBuffer(body))
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var bulkResp BulkTransferResponse
	json.NewDecoder(resp.Body).Decode(&bulkResp)
	success := "true"
	if !bulkResp.RequestSuccessful {
		success = "false"
	}
	observability.MonnifyCallsTotal.WithLabelValues("bulk_transfer", success).Inc()
	return &bulkResp, nil
}

type WalletBalanceResponse struct {
	RequestSuccessful bool `json:"requestSuccessful"`
	ResponseBody      []struct {
		WalletBalance float64 `json:"walletBalance"`
		WalletNumber  string  `json:"walletNumber"`
	} `json:"responseBody"`
}

// GetWalletBalance — source wallet balance in Kobo; converts the upstream float at the boundary.
func (c *Client) GetWalletBalance(walletNumber string) (money.Kobo, error) {
	start := time.Now()
	defer func() {
		observability.MonnifyCallDuration.WithLabelValues("wallet_balance").Observe(time.Since(start).Seconds())
	}()

	if c.MockMode {
		observability.MonnifyCallsTotal.WithLabelValues("wallet_balance", "true").Inc()
		return money.FromNaira(1_000_000), nil // ₦1,000,000.00
	}

	// Validate walletNumber before embedding in URL — prevents SSRF via injection
	if !walletNumberRe.MatchString(walletNumber) {
		return 0, fmt.Errorf("invalid wallet number format")
	}

	if err := c.Authenticate(); err != nil {
		return 0, err
	}

	endpoint := c.Config.BaseURL + "/api/v1/disbursements/wallet-balance?accountNumber=" + url.QueryEscape(walletNumber)
	req, _ := http.NewRequest("GET", endpoint, nil)
	req.Header.Set("Authorization", "Bearer "+c.AccessToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	var balanceResp WalletBalanceResponse
	json.NewDecoder(resp.Body).Decode(&balanceResp)

	if !balanceResp.RequestSuccessful || len(balanceResp.ResponseBody) == 0 {
		return 0, fmt.Errorf("failed to fetch wallet balance")
	}

	return money.FromNairaFloat(balanceResp.ResponseBody[0].WalletBalance), nil
}
