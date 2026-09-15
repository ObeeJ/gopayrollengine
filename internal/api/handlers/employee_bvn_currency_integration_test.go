//go:build integration

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/pkg/money"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedOrgWithCurrency creates a bare organization row with the given
// operating currency — the minimum CreateEmployee needs to resolve
// requiresBVN.
func seedOrgWithCurrency(t *testing.T, currency money.Currency) string {
	t.Helper()
	orgID := "ORG-" + uuid.New().String()[:8]
	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, currency, created_at, updated_at) VALUES (?, ?, ?, NOW(), NOW())",
		orgID, "currency test org", string(currency),
	).Error)
	return orgID
}

// newEmployeeCreateRequest builds a gin.Context carrying orgID the way
// JWTAuth + TenantMiddleware would, so CreateEmployee can be called directly
// without standing up the full middleware chain.
func newEmployeeCreateRequest(t *testing.T, orgID string, body map[string]any) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/employees", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(middleware.OrgIDKey, orgID)
	return w, c
}

func newEmployeeHandlerForTest() *EmployeeHandler {
	return NewEmployeeHandler(repository.NewEmployeeRepository(models.DB), services.NewEWAService())
}

// BVN is Nigeria's own KYC requirement — an NGN org must still supply one.
func TestCreateEmployee_NGNOrg_RequiresBVN(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	handler := newEmployeeHandlerForTest()

	w, c := newEmployeeCreateRequest(t, orgID, map[string]any{
		"name": "Ada", "email": "ada-" + uuid.New().String()[:8] + "@example.com",
		"account_number": "0123456789", "bank_code": "058", "salary": 300_000_00,
		// bvn intentionally omitted
	})
	handler.CreateEmployee(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateEmployee_NGNOrg_WithBVN_Succeeds(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	handler := newEmployeeHandlerForTest()

	w, c := newEmployeeCreateRequest(t, orgID, map[string]any{
		"name": "Ada", "email": "ada-" + uuid.New().String()[:8] + "@example.com",
		"account_number": "0123456789", "bank_code": "058", "salary": 300_000_00,
		"bvn": "12345678901",
	})
	handler.CreateEmployee(c)
	assert.Equal(t, http.StatusCreated, w.Code)
}

// A non-NGN org has no BVN to verify — requiring one would be a real
// onboarding blocker for an employee CBN has no jurisdiction over.
func TestCreateEmployee_NonNGNOrg_BVNNotRequired(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.GHS)
	handler := newEmployeeHandlerForTest()

	w, c := newEmployeeCreateRequest(t, orgID, map[string]any{
		"name": "Kwame", "email": "kwame-" + uuid.New().String()[:8] + "@example.com",
		"account_number": "0123456789", "bank_code": "058", "salary": 300_000_00,
		// bvn intentionally omitted — must not be required
	})
	handler.CreateEmployee(c)
	assert.Equal(t, http.StatusCreated, w.Code)
}
