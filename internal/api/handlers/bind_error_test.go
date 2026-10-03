package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go-payroll-engine/pkg/money"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type bindProbe struct {
	Email  string      `json:"contact_email" binding:"required,email"`
	Amount *money.Kobo `json:"amount" binding:"required"`
	Count  int         `json:"count"`
}

func bindOnce(t *testing.T, body string, limit int64) (int, map[string]any) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	if limit > 0 {
		c.Request.Body = http.MaxBytesReader(w, c.Request.Body, limit)
	}
	var req bindProbe
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBindError(c, err)
	} else {
		c.Status(http.StatusNoContent)
		c.Writer.WriteHeaderNow()
	}
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestRespondBindError_NamesFieldsInTheAPIsOwnVocabulary(t *testing.T) {
	code, out := bindOnce(t, `{}`, 0)
	assert.Equal(t, http.StatusBadRequest, code)
	fields, _ := out["fields"].(map[string]any)
	assert.Equal(t, "is required", fields["contact_email"], "JSON name, not the Go field name")
	assert.Equal(t, "is required", fields["amount"])
	msg, _ := out["error"].(string)
	for _, leak := range []string{"Key:", "Field validation", "bindProbe", "Email", "'Amount'", "binding"} {
		assert.NotContains(t, msg, leak, "validator/struct internals must not reach the client")
	}
}

func TestRespondBindError_ZeroIsAValueNotAMissingField(t *testing.T) {
	code, out := bindOnce(t, `{"contact_email":"a@b.ng","amount":0}`, 0)
	assert.Equal(t, http.StatusNoContent, code, "an explicit 0 must bind: %v", out)
}

func TestRespondBindError_BadEmailAndWrongType(t *testing.T) {
	code, out := bindOnce(t, `{"contact_email":"nope","amount":5}`, 0)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "must be a valid email address", out["fields"].(map[string]any)["contact_email"])

	code, out = bindOnce(t, `{"contact_email":"a@b.ng","amount":5,"count":"many"}`, 0)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, out["fields"].(map[string]any), "count")
}

func TestRespondBindError_MalformedAndEmptyBodies(t *testing.T) {
	for _, body := range []string{`{not json`, ``} {
		code, out := bindOnce(t, body, 0)
		assert.Equal(t, http.StatusBadRequest, code, body)
		assert.Equal(t, "request body must be valid JSON", out["error"], body)
	}
}

func TestRespondBindError_OversizedBodyIs413(t *testing.T) {
	code, out := bindOnce(t, `{"contact_email":"`+strings.Repeat("a", 4096)+`"}`, 64)
	require.Equal(t, http.StatusRequestEntityTooLarge, code)
	assert.Equal(t, "request body is too large", out["error"])
}
