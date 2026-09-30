//go:build integration

package workers

import (
	"context"
	"encoding/json"
	"errors"
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

// A 5xx (like a timeout or dropped connection) from the bulk-transfer call
// says nothing about whether Monnify accepted the batch. The worker must not
// mark it failed and let Asynq retry — each retry would resend every still-
// pending item, and whether employees got paid twice would rest entirely on
// Monnify rejecting the duplicate references. It must leave the batch
// processing for the webhooks to resolve, and must not be retried.
func TestProcessPayrollTask_AmbiguousOutcomeIsNotRetried(t *testing.T) {
	skipIfNoDB(t)
	const n = 3
	orgID, payrollID := seedPendingBatch(t, n)

	var batchCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"requestSuccessful": true,
			"responseBody":      map[string]interface{}{"accessToken": "t", "expiresIn": 3600},
		})
	})
	mux.HandleFunc("/api/v1/disbursements/batch", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&batchCalls, 1)
		w.WriteHeader(http.StatusBadGateway) // gateway gave up; Monnify may still have it
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

	err := h.ProcessPayrollTask(context.Background(), asynq.NewTask(TypeProcessPayroll, payload))
	require.Error(t, err)
	assert.True(t, errors.Is(err, asynq.SkipRetry), "an ambiguous outcome must never be auto-retried: %v", err)

	var p models.Payroll
	require.NoError(t, models.DB.First(&p, "id = ?", payrollID).Error)
	assert.Equal(t, models.PayrollProcessing, p.Status, "left for the webhooks to resolve, not failed")
	assert.Equal(t, n, p.PendingCount)
	assert.Equal(t, int32(1), atomic.LoadInt32(&batchCalls))
}
