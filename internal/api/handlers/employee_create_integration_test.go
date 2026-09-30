//go:build integration

package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func validEmployeeBody() map[string]any {
	return map[string]any{
		"name": "Chidi", "email": "chidi-" + uuid.New().String()[:8] + "@example.com",
		"account_number": "0123456789", "bank_code": "058", "salary": 250_000_00,
		"bvn": "12345678901",
	}
}

// An employer-onboarded employee previously never got a users row, so there
// was no way for them to log in to the worker app at all — only D2C signup
// created one.
func TestCreateEmployee_WithPhoneCreatesWorkerLogin(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	body := validEmployeeBody()
	body["phone"] = testPhone()

	w, c := newEmployeeCreateRequest(t, orgID, body)
	newEmployeeHandlerForTest().CreateEmployee(c)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

	var emp models.Employee
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &emp))
	var u models.User
	require.NoError(t, models.DB.First(&u, "employee_id = ?", emp.ID).Error)
	assert.Equal(t, body["phone"], u.Phone)
	assert.Equal(t, orgID, u.OrgID)
	assert.True(t, u.IsActive)
}

func TestCreateEmployee_DuplicatePhoneIs409AndRollsBack(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	phone := testPhone()

	first := validEmployeeBody()
	first["phone"] = phone
	w, c := newEmployeeCreateRequest(t, orgID, first)
	newEmployeeHandlerForTest().CreateEmployee(c)
	require.Equal(t, http.StatusCreated, w.Code)

	second := validEmployeeBody()
	second["phone"] = phone
	w, c = newEmployeeCreateRequest(t, orgID, second)
	newEmployeeHandlerForTest().CreateEmployee(c)
	assert.Equal(t, http.StatusConflict, w.Code)

	var n int64
	require.NoError(t, models.DB.Model(&models.Employee{}).
		Where("organization_id = ? AND email_hmac = ?", orgID, models.BlindIndex(second["email"].(string))).
		Count(&n).Error)
	assert.Zero(t, n, "the employee insert must roll back with the failed users insert")
}

func TestCreateEmployee_DuplicateEmailIs409(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	body := validEmployeeBody()

	w, c := newEmployeeCreateRequest(t, orgID, body)
	newEmployeeHandlerForTest().CreateEmployee(c)
	require.Equal(t, http.StatusCreated, w.Code)

	w, c = newEmployeeCreateRequest(t, orgID, body)
	newEmployeeHandlerForTest().CreateEmployee(c)
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestCreateEmployee_RejectsImpossibleBankDetailsAndPhone(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)

	for name, mutate := range map[string]func(map[string]any){
		"9-digit NUBAN":      func(b map[string]any) { b["account_number"] = "012345678" },
		"letters in NUBAN":   func(b map[string]any) { b["account_number"] = "01234567AB" },
		"non-numeric bank":   func(b map[string]any) { b["bank_code"] = "GTB" },
		"short BVN":          func(b map[string]any) { b["bvn"] = "1234" },
		"local-format phone": func(b map[string]any) { b["phone"] = "08012345678" },
	} {
		t.Run(name, func(t *testing.T) {
			body := validEmployeeBody()
			mutate(body)
			w, c := newEmployeeCreateRequest(t, orgID, body)
			newEmployeeHandlerForTest().CreateEmployee(c)
			assert.Equal(t, http.StatusBadRequest, w.Code, w.Body.String())
		})
	}
}

// Viewer and compliance roles can list staff; they must not get full
// account numbers. EncryptedString's MarshalJSON does the masking — this
// guards it from being bypassed by a future response DTO.
func TestGetEmployees_MasksAccountNumbers(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	w, c := newEmployeeCreateRequest(t, orgID, validEmployeeBody())
	newEmployeeHandlerForTest().CreateEmployee(c)
	require.Equal(t, http.StatusCreated, w.Code)

	lw := httptest.NewRecorder()
	lc, _ := gin.CreateTestContext(lw)
	lc.Request = httptest.NewRequest(http.MethodGet, "/api/v1/employees", nil)
	lc.Set(middleware.OrgIDKey, orgID)
	newEmployeeHandlerForTest().GetEmployees(lc)
	require.Equal(t, http.StatusOK, lw.Code)

	assert.NotContains(t, lw.Body.String(), "0123456789")
	assert.Contains(t, lw.Body.String(), "6789")
}
