//go:build integration

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// payrollRouter mounts the real handlers on the real route shapes, with the
// tenant and role taken from headers (standing in for JWT + tenant middleware),
// so path params, binding and status mapping are all exercised.
func payrollRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		c.Set(middleware.OrgIDKey, c.GetHeader("X-Org"))
		c.Set("role", "admin")
		c.Next()
	})
	h := &PayrollHandler{Service: newPayrollSvc()}
	r.GET("/payrolls/", h.ListPayrolls)
	r.GET("/payrolls/:id", h.GetPayroll)
	r.POST("/payrolls/:id/retry", h.RetryPayroll)
	r.POST("/payrolls/:id/items/:item_id/resolve", h.ResolvePayrollItem)
	return r
}

func call(t *testing.T, r *gin.Engine, method, path, org, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("X-Org", org)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func TestPayrollHTTP_GetShowsOutcomeAndListShowsHistory(t *testing.T) {
	skipIfNoDB(t)
	newRecordingBank(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, _ := partiallyPaidBatch(t, orgID)
	r := payrollRouter()

	code, out := call(t, r, http.MethodGet, "/payrolls/"+payrollID, orgID, "")
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, "failed", out["status"], "the stored state is unchanged")
	summary := out["summary"].(map[string]any)
	assert.Equal(t, "partially_paid", summary["outcome"], "…but a person reads it as partially paid")
	assert.EqualValues(t, 2, summary["paid_items"])
	assert.EqualValues(t, 1, summary["failed_items"])
	assert.Len(t, out["items"], 3)

	code, out = call(t, r, http.MethodGet, "/payrolls/?page=1&page_size=10", orgID, "")
	require.Equal(t, http.StatusOK, code)
	assert.EqualValues(t, 1, out["total"])
	data := out["data"].([]any)
	require.Len(t, data, 1)
	entry := data[0].(map[string]any)
	assert.Equal(t, payrollID, entry["id"])
	assert.NotContains(t, entry, "items", "history rows stay small")
	assert.Equal(t, "partially_paid", entry["summary"].(map[string]any)["outcome"])

	code, _ = call(t, r, http.MethodGet, "/payrolls/PAY-nosuch", orgID, "")
	assert.Equal(t, http.StatusNotFound, code)
	other := seedOrgWithCurrency(t, money.NGN)
	code, _ = call(t, r, http.MethodGet, "/payrolls/"+payrollID, other, "")
	assert.Equal(t, http.StatusNotFound, code, "another tenant's batch looks like no batch")
}

func TestPayrollHTTP_ListClampsPageSize(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	code, out := call(t, payrollRouter(), http.MethodGet, "/payrolls/?page_size=100000&page=-4", orgID, "")
	require.Equal(t, http.StatusOK, code)
	assert.EqualValues(t, 20, out["page_size"], "an out-of-range page size falls back to the default")
	assert.EqualValues(t, 1, out["page"])
}

func TestPayrollHTTP_RetryStatusMapping(t *testing.T) {
	skipIfNoDB(t)
	newRecordingBank(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, emeka := partiallyPaidBatch(t, orgID)
	r := payrollRouter()

	code, _ := call(t, r, http.MethodPost, "/payrolls/PAY-nosuch/retry", orgID, "")
	assert.Equal(t, http.StatusNotFound, code)

	code, out := call(t, r, http.MethodPost, "/payrolls/"+payrollID+"/retry", orgID, "")
	require.Equal(t, http.StatusAccepted, code, "%v", out)
	assert.EqualValues(t, 1, out["retrying"])

	// The retry is now in flight; the batch is failed-with-pending until the worker runs.
	// A second call resumes it (queues again) rather than failing: the line is pending.
	code, out = call(t, r, http.MethodPost, "/payrolls/"+payrollID+"/retry", orgID, "")
	assert.Equal(t, http.StatusAccepted, code, "resuming is idempotent: %v", out)
	assert.Equal(t, 2, itemByName(t, payrollID, "Emeka").Attempt, "resuming must not burn another attempt")

	runWorker(t, orgID, payrollID)
	code, _ = call(t, r, http.MethodPost, "/payrolls/"+payrollID+"/retry", orgID, "")
	assert.Equal(t, http.StatusConflict, code, "a batch that is processing again cannot be retried")

	require.Equal(t, http.StatusOK, deliver(t, emeka.ID+"-R2", "DISBURSEMENT_SUCCESSFUL", emeka.Amount))
	code, _ = call(t, r, http.MethodPost, "/payrolls/"+payrollID+"/retry", orgID, "")
	assert.Equal(t, http.StatusConflict, code, "a paid batch cannot be retried")
}

func TestPayrollHTTP_ResolveStatusMapping(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, it := stuckItem(t, orgID)
	r := payrollRouter()
	path := "/payrolls/" + payrollID + "/items/" + it.ID + "/resolve"

	code, out := call(t, r, http.MethodPost, path, orgID, `{"outcome":"paid"}`)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Equal(t, "is required", out["fields"].(map[string]any)["note"])
	code, _ = call(t, r, http.MethodPost, path, orgID, `{"outcome":"maybe","note":"hmm"}`)
	assert.Equal(t, http.StatusBadRequest, code)
	code, out = call(t, r, http.MethodPost, path, orgID, `{"outcome":"paid","note":"saw it on the dashboard"}`)
	assert.Equal(t, http.StatusConflict, code, "not overdue yet")
	assert.Contains(t, out["error"], "not overdue")

	backdateSent(t, it.ID, models.StuckAfter+time.Minute)
	other := seedOrgWithCurrency(t, money.NGN)
	code, _ = call(t, r, http.MethodPost, path, other, `{"outcome":"paid","note":"hostile"}`)
	assert.Equal(t, http.StatusNotFound, code)

	code, out = call(t, r, http.MethodPost, path, orgID, `{"outcome":"paid","note":"saw it on the dashboard","evidence":"MNFY-9"}`)
	require.Equal(t, http.StatusOK, code, "%v", out)
	assert.Equal(t, "completed", out["status"])
	assert.Equal(t, "MNFY-9", out["resolution_evidence"])

	code, _ = call(t, r, http.MethodPost, path, orgID, `{"outcome":"failed","note":"changed my mind","evidence":"stmt"}`)
	assert.Equal(t, http.StatusConflict, code, "already settled")
}

func TestPayrollHTTP_StuckBatchIsFlaggedInTheSummary(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, it := stuckItem(t, orgID)
	r := payrollRouter()

	_, out := call(t, r, http.MethodGet, "/payrolls/"+payrollID, orgID, "")
	assert.Equal(t, false, out["summary"].(map[string]any)["stuck"])
	backdateSent(t, it.ID, models.StuckAfter+time.Minute)
	_, out = call(t, r, http.MethodGet, "/payrolls/"+payrollID, orgID, "")
	assert.Equal(t, true, out["summary"].(map[string]any)["stuck"], "an overdue callback must be visible to the employer")
}
