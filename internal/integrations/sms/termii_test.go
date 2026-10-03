package sms

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sendRequest is the wire body as the gateway receives it (decode only).
type sendRequest struct {
	To      string `json:"to"`
	From    string `json:"from"`
	SMS     string `json:"sms"`
	Type    string `json:"type"`
	Channel string `json:"channel"`
	APIKey  string `json:"api_key"`
}

func fakeGateway(t *testing.T, status int, reply string, got *sendRequest) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/api/sms/send", r.URL.Path)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		if got != nil {
			require.NoError(t, json.NewDecoder(r.Body).Decode(got))
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(reply))
	}))
	t.Cleanup(srv.Close)
	return New("key-123", "PAYROLL", srv.URL+"/")
}

func TestSendOTP_SendsExpectedRequest(t *testing.T) {
	var got sendRequest
	c := fakeGateway(t, 200, `{"code":"ok","message_id":"m1","message":"Successfully Sent"}`, &got)

	require.NoError(t, c.SendOTP(context.Background(), "+2348012345678", "493021"))

	assert.Equal(t, "2348012345678", got.To, "digits only, no plus")
	assert.Equal(t, "PAYROLL", got.From)
	assert.Equal(t, "key-123", got.APIKey)
	assert.Equal(t, "plain", got.Type)
	assert.Contains(t, got.SMS, "493021")
}

func TestSendText_GatewayFailuresAreErrorsAndLeakNothing(t *testing.T) {
	cases := map[string]struct {
		status int
		reply  string
	}{
		"http error":              {500, `{"message":"boom"}`},
		"rejected with 2xx":       {200, `{"code":"error","message":"insufficient balance"}`},
		"2xx without ok":          {200, `{}`},
		"unauthorised":            {401, `{"message":"Invalid API key"}`},
		"non-json 2xx":            {200, `<html>maintenance</html>`},
		"ok in wrong status band": {302, `{"code":"ok"}`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			c := fakeGateway(t, tc.status, tc.reply, nil)
			err := c.SendText(context.Background(), "+2348012345678", "SECRET-BODY-493021")
			require.Error(t, err)
			assert.NotContains(t, err.Error(), "key-123", "the API key must never appear in an error")
			assert.NotContains(t, err.Error(), "SECRET-BODY-493021", "the message body must never appear in an error")
		})
	}
}

func TestSendText_UnreachableGateway(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	err := New("key-123", "PAYROLL", url).SendText(context.Background(), "+2348012345678", "x")
	require.Error(t, err)
	assert.NotContains(t, err.Error(), "key-123")
}

func TestSendText_HonoursContext(t *testing.T) {
	c := fakeGateway(t, 200, `{"code":"ok"}`, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Error(t, c.SendText(ctx, "+2348012345678", "x"))
}

func TestFromEnv(t *testing.T) {
	t.Setenv("TERMII_API_KEY", "")
	t.Setenv("TERMII_SENDER_ID", "")
	_, err := FromEnv()
	assert.ErrorIs(t, err, ErrNotConfigured)

	t.Setenv("TERMII_API_KEY", "k")
	_, err = FromEnv()
	assert.ErrorIs(t, err, ErrNotConfigured, "both key and sender id are required")

	t.Setenv("TERMII_SENDER_ID", "S")
	c, err := FromEnv()
	require.NoError(t, err)
	assert.Equal(t, defaultBaseURL, c.baseURL)

	t.Setenv("TERMII_BASE_URL", "http://localhost:9/")
	c, err = FromEnv()
	require.NoError(t, err)
	assert.Equal(t, "http://localhost:9", c.baseURL)
}
