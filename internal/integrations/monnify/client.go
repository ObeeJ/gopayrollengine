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
	defer func() { _ = resp.Body.Close() }()

	var authResp AuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&authResp); err != nil {
		return fmt.Errorf("monnify auth response decode failed: %w", err)
	}

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
	defer func() { _ = resp.Body.Close() }()

	var bulkResp BulkTransferResponse
	if err := json.NewDecoder(resp.Body).Decode(&bulkResp); err != nil {
		return nil, fmt.Errorf("monnify bulk transfer response decode failed: %w", err)
	}
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
	defer func() { _ = resp.Body.Close() }()

	var balanceResp WalletBalanceResponse
	if err := json.NewDecoder(resp.Body).Decode(&balanceResp); err != nil {
		return 0, fmt.Errorf("monnify wallet balance response decode failed: %w", err)
	}

	if !balanceResp.RequestSuccessful || len(balanceResp.ResponseBody) == 0 {
		return 0, fmt.Errorf("failed to fetch wallet balance")
	}

	return money.FromNairaFloat(balanceResp.ResponseBody[0].WalletBalance), nil
}

// ReserveAccountRequest asks Monnify to mint a dedicated ("reserved") account
// number that credits land in when a customer — here, an employer funding
// their EWA pool — deposits into it.
//
// developers.monnify.com was unreachable from the environment this was built
// in (egress-blocked), so this shape follows Monnify's long-documented
// Reserved Accounts convention rather than a direct read of current docs —
// the same limitation and the same resolution as the Paystack client
// (internal/integrations/paystack/client.go).
type ReserveAccountRequest struct {
	AccountReference string `json:"accountReference"`
	AccountName      string `json:"accountName"`
	CurrencyCode     string `json:"currencyCode"`
	ContractCode     string `json:"contractCode"`
	CustomerEmail    string `json:"customerEmail"`
	CustomerName     string `json:"customerName"`
	// GetAllAvailableBanks requests one number per partner bank; the adapter
	// only needs one, so this is left false and Monnify returns its default.
	GetAllAvailableBanks bool `json:"getAllAvailableBanks"`
}

type reservedAccountDetail struct {
	BankName      string `json:"bankName"`
	BankCode      string `json:"bankCode"`
	AccountNumber string `json:"accountNumber"`
}

type ReserveAccountResponse struct {
	RequestSuccessful bool   `json:"requestSuccessful"`
	ResponseMessage   string `json:"responseMessage"`
	ResponseBody      struct {
		AccountReference string                  `json:"accountReference"`
		AccountName      string                  `json:"accountName"`
		Accounts         []reservedAccountDetail `json:"accounts"`
	} `json:"responseBody"`
}

// CreateReservedAccount provisions the dedicated account. Idempotent on
// Monnify's side by AccountReference: calling this again for an org that
// already has one returns the same account rather than minting a second.
func (c *Client) CreateReservedAccount(req ReserveAccountRequest) (*ReserveAccountResponse, error) {
	start := time.Now()
	defer func() {
		observability.MonnifyCallDuration.WithLabelValues("reserved_account").Observe(time.Since(start).Seconds())
	}()

	if c.MockMode {
		observability.MonnifyCallsTotal.WithLabelValues("reserved_account", "true").Inc()
		resp := &ReserveAccountResponse{RequestSuccessful: true, ResponseMessage: "Mock Account Created"}
		resp.ResponseBody.AccountReference = req.AccountReference
		resp.ResponseBody.AccountName = req.AccountName
		resp.ResponseBody.Accounts = []reservedAccountDetail{
			{BankName: "Mock Bank", BankCode: "000", AccountNumber: "0000000000"},
		}
		return resp, nil
	}
	if err := c.Authenticate(); err != nil {
		observability.MonnifyCallsTotal.WithLabelValues("reserved_account", "false").Inc()
		return nil, err
	}

	body, _ := json.Marshal(req)
	httpReq, _ := http.NewRequest("POST", c.Config.BaseURL+"/api/v2/bank-transfer/reserved-accounts", bytes.NewBuffer(body))
	httpReq.Header.Set("Authorization", "Bearer "+c.AccessToken)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		observability.MonnifyCallsTotal.WithLabelValues("reserved_account", "false").Inc()
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	var accResp ReserveAccountResponse
	if err := json.NewDecoder(resp.Body).Decode(&accResp); err != nil {
		observability.MonnifyCallsTotal.WithLabelValues("reserved_account", "false").Inc()
		return nil, fmt.Errorf("monnify reserved account response decode failed: %w", err)
	}
	success := "true"
	if !accResp.RequestSuccessful {
		success = "false"
	}
	observability.MonnifyCallsTotal.WithLabelValues("reserved_account", success).Inc()
	return &accResp, nil
}
