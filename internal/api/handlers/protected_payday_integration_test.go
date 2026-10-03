//go:build integration

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/pkg/money"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func postAsWorker(t *testing.T, h gin.HandlerFunc, orgID, employeeID, body string) (int, map[string]any) {
	t.Helper()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(middleware.OrgIDKey, orgID)
	c.Set("employee_id", employeeID)
	h(c)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// "protect ₦0" is how a worker removes their floor. The request used to be
// bound with `binding:"required"` on a money.Kobo, and gin's "required" treats
// the zero value as missing, so the one amount that means "no floor" was
// rejected with a raw validator message.
func TestSetProtectedPayday_ZeroRemovesTheFloor(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	empID := seedActiveSalaried(t, orgID, "Hauwa", money.FromNaira(150_000))
	ewa := services.NewEWAService()
	noCooling := 0
	_, err := ewa.UpdatePolicy(context.Background(), orgID, services.PolicyUpdate{CoolingOffHours: &noCooling}, "127.0.0.1")
	require.NoError(t, err)
	h := NewAdvanceHandler(ewa).SetProtectedPayday

	code, out := postAsWorker(t, h, orgID, empID, `{"amount": 3000000}`)
	require.Equal(t, http.StatusOK, code, "%v", out)
	assert.EqualValues(t, 3000000, out["protected_payday"])

	code, out = postAsWorker(t, h, orgID, empID, `{"amount": 0}`)
	require.Equal(t, http.StatusOK, code, "an explicit 0 removes the floor: %v", out)
	assert.EqualValues(t, 0, out["protected_payday"])

	var pref models.EWAWorkerPreference
	require.NoError(t, models.DB.First(&pref, "employee_id = ?", empID).Error)
	assert.Zero(t, pref.ProtectedPaydayMinor)
}

func TestSetProtectedPayday_MissingOrNegativeAmountIsStillRefused(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	empID := seedActiveSalaried(t, orgID, "Ibrahim", money.FromNaira(120_000))
	h := NewAdvanceHandler(services.NewEWAService()).SetProtectedPayday

	code, out := postAsWorker(t, h, orgID, empID, `{}`)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "is required", out["fields"].(map[string]any)["amount"],
		"leaving the field out must stay an error, distinct from an explicit 0")

	code, _ = postAsWorker(t, h, orgID, empID, `{"amount": -5}`)
	assert.Equal(t, http.StatusBadRequest, code)
}
