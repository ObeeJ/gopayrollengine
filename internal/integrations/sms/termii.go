// Package sms sends text messages through a Termii-style HTTP gateway: the
// worker login code, and notices such as "your bank details were changed".
//
// Verification note: Termii's documentation was unreachable from the
// environment this was written in (egress-blocked), so the request shape
// below follows its long-standing public "send message" convention
// (POST /api/sms/send, JSON body with api_key/to/from/sms/type/channel,
// answered with {"code":"ok","message_id":...}) rather than a direct read of
// the current docs. It is tested against a fake server for our own behaviour;
// confirm one real send in staging before relying on it for logins.
package sms

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	defaultBaseURL = "https://api.ng.termii.com"
	httpTimeout    = 10 * time.Second
	maxRespBytes   = 64 << 10
)

// ErrNotConfigured is returned by FromEnv when no gateway credentials are set.
var ErrNotConfigured = errors.New("sms: TERMII_API_KEY and TERMII_SENDER_ID are not set")

// Client sends SMS through the gateway.
type Client struct {
	apiKey   string
	senderID string
	baseURL  string
	http     *http.Client
}

// New builds a client. baseURL may be empty (production default).
func New(apiKey, senderID, baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{apiKey: apiKey, senderID: senderID, baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{Timeout: httpTimeout}}
}

// FromEnv builds a client from TERMII_API_KEY, TERMII_SENDER_ID and the
// optional TERMII_BASE_URL, or returns ErrNotConfigured.
func FromEnv() (*Client, error) {
	key, sender := os.Getenv("TERMII_API_KEY"), os.Getenv("TERMII_SENDER_ID")
	if key == "" || sender == "" {
		return nil, ErrNotConfigured
	}
	return New(key, sender, os.Getenv("TERMII_BASE_URL")), nil
}

type sendResponse struct {
	Code      string `json:"code"`
	MessageID string `json:"message_id"`
	Message   string `json:"message"`
}

// SendText delivers text to an E.164 phone number ("+2348012345678"). The
// returned error never contains the message body or the API key.
func (c *Client) SendText(ctx context.Context, phone, text string) error {
	// The gateway's protocol carries the key in the JSON body. A map, not a
	// struct with an APIKey field, so a secret-named field is never marshaled
	// by a type that could be logged or reused elsewhere.
	body, err := json.Marshal(map[string]string{
		"to":      strings.TrimPrefix(phone, "+"), // the gateway wants digits only
		"from":    c.senderID,
		"sms":     text,
		"type":    "plain",
		"channel": "generic",
		"api_key": c.apiKey,
	})
	if err != nil {
		return fmt.Errorf("sms: encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/sms/send", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("sms: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		// url.Error embeds the URL, which has no secrets here (the key is in
		// the body), but keep the message minimal anyway.
		return errors.New("sms: gateway unreachable")
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxRespBytes))

	var out sendResponse
	_ = json.Unmarshal(raw, &out)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("sms: gateway answered %d: %s", resp.StatusCode, clip(out.Message))
	}
	// A 2xx without the gateway's own "ok" is not proof of delivery; treat it as a failure so
	// the caller does not tell a worker a code is on its way when it is not.
	if !strings.EqualFold(out.Code, "ok") {
		return fmt.Errorf("sms: gateway did not accept the message (code %q): %s", clip(out.Code), clip(out.Message))
	}
	return nil
}

// SendOTP satisfies services.OTPSender.
func (c *Client) SendOTP(ctx context.Context, phone, code string) error {
	return c.SendText(ctx, phone, fmt.Sprintf("Your login code is %s. It expires in 5 minutes. Never share it with anyone.", code))
}

func clip(s string) string {
	if len(s) > 120 {
		return s[:120]
	}
	return s
}
