// Package paystack is a minimal Paystack Transfers client: recipient creation
// plus transfer initiation, the two calls a disbursement needs. It exists
// alongside the Monnify client so this codebase has a second, independently
// operable NGN rail — a single payment provider is a single point of failure
// for every salary the system moves.
//
// Verification note: developers.paystack.co was unreachable from the
// environment this was built in (egress-blocked), so the request/response
// shapes below follow Paystack's long-standing, widely-documented public API
// conventions rather than a direct read of current docs. Confirm against the
// live API reference before this handles real transfers.
package paystack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go-payroll-engine/internal/observability"
	"net/http"
	"os"
	"time"
)

const httpTimeout = 30 * time.Second

type Config struct {
	SecretKey string
	BaseURL   string
}

type Client struct {
	Config     Config
	MockMode   bool
	httpClient *http.Client
}

// NewClient builds the Paystack client from env. MOCK_MODE=true short-circuits
// real calls, matching the convention every other provider client in this
// codebase uses.
func NewClient() *Client {
	baseURL := os.Getenv("PAYSTACK_BASE_URL")
	if baseURL == "" {
		baseURL = "https://api.paystack.co"
	}
	return &Client{
		Config: Config{
			SecretKey: os.Getenv("PAYSTACK_SECRET_KEY"),
			BaseURL:   baseURL,
		},
		MockMode:   os.Getenv("MOCK_MODE") == "true",
		httpClient: &http.Client{Timeout: httpTimeout},
	}
}

// CreateRecipientRequest registers a bank account as a transfer destination.
// Paystack requires a recipient_code before a transfer can be initiated —
// there is no single-call "pay this account number" endpoint.
type CreateRecipientRequest struct {
	// Type selects the payout rail Paystack uses to interpret AccountNumber /
	// BankCode. "nuban" is Nigeria's Nomination Unified Bank Account Number
	// scheme — the only value this client has been exercised against.
	// Ghana/Kenya/South Africa each use a different recipient type
	// ("ghipss", "mobile_money", "basa" per Paystack convention), which this
	// client does not yet map — see the currency-support note on the adapter.
	Type          string `json:"type"`
	Name          string `json:"name"`
	AccountNumber string `json:"account_number"`
	BankCode      string `json:"bank_code"`
	Currency      string `json:"currency"`
}

type recipientResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    struct {
		RecipientCode string `json:"recipient_code"`
	} `json:"data"`
}

// CreateRecipient registers a recipient and returns Paystack's recipient_code.
func (c *Client) CreateRecipient(ctx context.Context, req CreateRecipientRequest) (string, error) {
	start := time.Now()
	defer func() {
		observability.ProviderCallDuration.WithLabelValues("paystack", "create_recipient").Observe(time.Since(start).Seconds())
	}()

	if c.MockMode {
		return "RCP_mock_" + req.AccountNumber, nil
	}

	var resp recipientResponse
	if err := c.post(ctx, "/transferrecipient", req, &resp); err != nil {
		return "", err
	}
	if !resp.Status {
		return "", fmt.Errorf("paystack: create recipient failed: %s", resp.Message)
	}
	return resp.Data.RecipientCode, nil
}

// InitiateTransferRequest moves money to a previously created recipient.
type InitiateTransferRequest struct {
	// Source is always "balance" for a standard payout — Paystack's own
	// convention for "pay from the merchant's Paystack balance".
	Source    string `json:"source"`
	Amount    int64  `json:"amount"` // minor units — kobo, pesewas, cents
	Recipient string `json:"recipient"`
	Reason    string `json:"reason"`
	Reference string `json:"reference"`
	Currency  string `json:"currency"`
}

type TransferResponse struct {
	Status  bool   `json:"status"`
	Message string `json:"message"`
	Data    struct {
		TransferCode string `json:"transfer_code"`
		Reference    string `json:"reference"`
		Status       string `json:"status"` // "pending" | "success" | "failed" | "otp" | "reversed"
	} `json:"data"`
}

// InitiateTransfer submits the payout. A transfer requiring OTP confirmation
// ("status": "otp" in the response) is a configuration problem, not a runtime
// one — production integrations disable OTP for API-initiated transfers in the
// Paystack dashboard, since an unattended worker cannot supply one. That "otp"
// status is surfaced via the returned status field rather than silently
// swallowed, so misconfiguration fails loudly.
func (c *Client) InitiateTransfer(ctx context.Context, req InitiateTransferRequest) (*TransferResponse, error) {
	start := time.Now()
	defer func() {
		observability.ProviderCallDuration.WithLabelValues("paystack", "transfer").Observe(time.Since(start).Seconds())
	}()

	if c.MockMode {
		observability.ProviderCallsTotal.WithLabelValues("paystack", "transfer", "true").Inc()
		resp := &TransferResponse{Status: true, Message: "Mock transfer queued"}
		resp.Data.TransferCode = "TRF_mock_" + req.Reference
		resp.Data.Reference = req.Reference
		resp.Data.Status = "success"
		return resp, nil
	}

	var resp TransferResponse
	if err := c.post(ctx, "/transfer", req, &resp); err != nil {
		observability.ProviderCallsTotal.WithLabelValues("paystack", "transfer", "false").Inc()
		return nil, err
	}
	success := "true"
	if !resp.Status {
		success = "false"
	}
	observability.ProviderCallsTotal.WithLabelValues("paystack", "transfer", success).Inc()
	return &resp, nil
}

// VerifyTransfer polls the current status of a transfer by its reference.
func (c *Client) VerifyTransfer(ctx context.Context, reference string) (*TransferResponse, error) {
	if c.MockMode {
		resp := &TransferResponse{Status: true}
		resp.Data.Reference = reference
		resp.Data.Status = "success"
		return resp, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.Config.BaseURL+"/transfer/verify/"+reference, nil)
	if err != nil {
		return nil, fmt.Errorf("paystack: build verify request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Config.SecretKey)

	httpResp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("paystack: verify transfer request failed: %w", err)
	}
	defer func() { _ = httpResp.Body.Close() }()

	var resp TransferResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&resp); err != nil {
		return nil, fmt.Errorf("paystack: verify transfer response decode failed: %w", err)
	}
	return &resp, nil
}

func (c *Client) post(ctx context.Context, path string, payload, out interface{}) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("paystack: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.Config.BaseURL+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("paystack: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Config.SecretKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("paystack: request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("paystack: response decode failed: %w", err)
	}
	return nil
}
