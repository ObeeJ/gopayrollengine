//go:build integration

package workers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/models"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// When the bank definitively refuses a batch (e.g. insufficient wallet
// balance), nothing was sent. The items must go back to 'pending' along with
// the batch moving to 'failed', so that the asynq retry (failed → processing)
// submits them. Items left 'processing' would be skipped by the retry as
// already sent, and the batch would be stranded.
func TestProcessPayrollTask_DefiniteRejectionReleasesItemsForRetry(t *testing.T) {
	skipIfNoDB(t)
	const n = 3
	orgID, payrollID := seedPendingBatch(t, n)

	var accept atomic.Bool
	var calls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"requestSuccessful": true,
			"responseBody":      map[string]interface{}{"accessToken": "t", "expiresIn": 3600},
		})
	})
	mux.HandleFunc("/api/v1/disbursements/batch", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		var body monnify.BulkTransferRequest
		_ = json.NewDecoder(r.Body).Decode(&body)
		if !accept.Load() {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"requestSuccessful": false, "responseMessage": "insufficient wallet balance",
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"requestSuccessful": true, "responseMessage": "ok",
			"responseBody": map[string]interface{}{"batchReference": body.BatchReference, "status": "SUCCESSFUL"},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	t.Setenv("MOCK_MODE", "false")
	t.Setenv("MONNIFY_BASE_URL", srv.URL)
	t.Setenv("MONNIFY_API_KEY", "test")
	t.Setenv("MONNIFY_SECRET_KEY", "test")
	t.Setenv("MONNIFY_SOURCE_WALLET", "9999999999")

	h := &PayrollHandler{MonnifyClient: monnify.NewClient()}
	payload, _ := json.Marshal(map[string]string{"payroll_id": payrollID, "org_id": orgID})
	run := func() error {
		return h.ProcessPayrollTask(context.Background(), asynq.NewTask(TypeProcessPayroll, payload))
	}

	// 1. The bank says no.
	require.Error(t, run())
	var p models.Payroll
	require.NoError(t, models.DB.First(&p, "id = ?", payrollID).Error)
	assert.Equal(t, models.PayrollFailed, p.Status)
	for _, it := range itemsFor(t, payrollID) {
		assert.Equal(t, models.PayrollPending, it.Status, "nothing was sent, so %s goes back to pending", it.ID)
	}

	// 2. The asynq retry, after the wallet is topped up, must send all of them.
	accept.Store(true)
	require.NoError(t, run())
	require.NoError(t, models.DB.First(&p, "id = ?", payrollID).Error)
	assert.Equal(t, models.PayrollProcessing, p.Status)
	assert.Equal(t, n, p.PendingCount, "the retry must submit every released item")
	for _, it := range itemsFor(t, payrollID) {
		assert.Equal(t, models.PayrollProcessing, it.Status)
	}
	assert.Equal(t, int32(2), atomic.LoadInt32(&calls))
}
