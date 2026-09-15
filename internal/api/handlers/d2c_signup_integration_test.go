//go:build integration

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-payroll-engine/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func d2cSignupRequest(t *testing.T, body map[string]any) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/d2c/signup", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	return w, c
}

func validD2CSignupBody() map[string]any {
	return map[string]any{
		"name":                "Ada Worker",
		"phone":               "+234" + uuid.New().String()[:10],
		"email":               "ada-" + uuid.New().String()[:8] + "@example.com",
		"account_number":      "0123456789",
		"bank_code":           "058",
		"bvn":                 "12345678901",
		"accepted_disclosure": true,
	}
}

func TestD2CSignup_CreatesOrgEmployeeUserAndReturnsWorkerToken(t *testing.T) {
	skipIfNoDB(t)
	body := validD2CSignupBody()
	w, c := d2cSignupRequest(t, body)

	(&D2CHandler{}).Signup(c)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var resp struct {
		Token      string `json:"token"`
		OrgID      string `json:"org_id"`
		EmployeeID string `json:"employee_id"`
		Role       string `json:"role"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.Token)
	assert.NotEmpty(t, resp.OrgID)
	assert.NotEmpty(t, resp.EmployeeID)
	assert.Equal(t, "employee", resp.Role)

	var org models.Organization
	require.NoError(t, models.DB.First(&org, "id = ?", resp.OrgID).Error)
	assert.True(t, org.IsD2C)
	assert.Equal(t, "Ada Worker", org.Name)

	var emp models.Employee
	require.NoError(t, models.DB.First(&emp, "id = ?", resp.EmployeeID).Error)
	assert.Equal(t, resp.OrgID, emp.OrganizationID)

	var user models.User
	require.NoError(t, models.DB.First(&user, "employee_id = ?", resp.EmployeeID).Error)
	assert.Equal(t, body["phone"], user.Phone)
	assert.Equal(t, resp.OrgID, user.OrgID)

	var consents []models.ConsentRecord
	require.NoError(t, models.DB.Where("employee_id = ?", resp.EmployeeID).Find(&consents).Error)
	require.Len(t, consents, 1)
	assert.Equal(t, "d2c_direct_debit_disclosure", consents[0].ConsentType)
	assert.True(t, consents[0].Granted)
}

func TestD2CSignup_RejectsMissingDisclosureAcceptance(t *testing.T) {
	skipIfNoDB(t)
	body := validD2CSignupBody()
	body["accepted_disclosure"] = false
	w, c := d2cSignupRequest(t, body)

	(&D2CHandler{}).Signup(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestD2CSignup_RequiresBVNForNGN(t *testing.T) {
	skipIfNoDB(t)
	body := validD2CSignupBody()
	delete(body, "bvn")
	w, c := d2cSignupRequest(t, body)

	(&D2CHandler{}).Signup(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestD2CSignup_NonNGNCurrencySkipsBVNRequirement(t *testing.T) {
	skipIfNoDB(t)
	body := validD2CSignupBody()
	delete(body, "bvn")
	body["currency"] = "USD"
	w, c := d2cSignupRequest(t, body)

	(&D2CHandler{}).Signup(c)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var resp struct {
		OrgID string `json:"org_id"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))

	var org models.Organization
	require.NoError(t, models.DB.First(&org, "id = ?", resp.OrgID).Error)
	assert.Equal(t, "USD", string(org.Currency))
}

func TestD2CSignup_RejectsInvalidCurrency(t *testing.T) {
	skipIfNoDB(t)
	body := validD2CSignupBody()
	body["currency"] = "NOTACURRENCY"
	w, c := d2cSignupRequest(t, body)

	(&D2CHandler{}).Signup(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// A second signup with a phone number already in use must fail outright,
// never silently attach a second worker identity to someone else's org.
func TestD2CSignup_DuplicatePhoneFails(t *testing.T) {
	skipIfNoDB(t)
	body := validD2CSignupBody()
	w1, c1 := d2cSignupRequest(t, body)
	(&D2CHandler{}).Signup(c1)
	require.Equal(t, http.StatusCreated, w1.Code, w1.Body.String())

	body2 := validD2CSignupBody()
	body2["phone"] = body["phone"]
	w2, c2 := d2cSignupRequest(t, body2)
	(&D2CHandler{}).Signup(c2)
	assert.Equal(t, http.StatusInternalServerError, w2.Code)
}

func TestD2CSignup_RejectsMissingRequiredFields(t *testing.T) {
	skipIfNoDB(t)
	for _, field := range []string{"name", "phone", "email", "account_number", "bank_code"} {
		body := validD2CSignupBody()
		delete(body, field)
		w, c := d2cSignupRequest(t, body)
		(&D2CHandler{}).Signup(c)
		assert.Equal(t, http.StatusBadRequest, w.Code, "missing %s should 400", field)
	}
}
