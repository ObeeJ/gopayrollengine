//go:build integration

package workers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/models"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingMonnify captures the item references submitted on each bulk transfer
// so a test can assert exactly which items were sent to be paid.
type recordingMonnify struct {
	server *httptest.Server
	mu     sync.Mutex
	// submitted holds the "reference" of every transfer line, across all calls.
	submitted []string
	calls     int
}

func newRecordingMonnify(t *testing.T) *recordingMonnify {
	t.Helper()
	rm := &recordingMonnify{}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"requestSuccessful": true,
			"responseBody":      map[string]interface{}{"accessToken": "t", "expiresIn": 3600},
		})
	})
	mux.HandleFunc("/api/v1/disbursements/batch", func(w http.ResponseWriter, r *http.Request) {
		var body monnify.BulkTransferRequest
		_ = json.NewDecoder(r.Body).Decode(&body)

		rm.mu.Lock()
		rm.calls++
		for _, line := range body.TransactionList {
			rm.submitted = append(rm.submitted, line.Reference)
		}
		rm.mu.Unlock()

		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"requestSuccessful": true,
			"responseMessage":   "ok",
			"responseBody":      map[string]interface{}{"batchReference": body.BatchReference, "status": "SUCCESSFUL"},
		})
	})

	rm.server = httptest.NewServer(mux)
	t.Cleanup(rm.server.Close)
	return rm
}

func (rm *recordingMonnify) references() []string {
	rm.mu.Lock()
	defer rm.mu.Unlock()
	out := make([]string, len(rm.submitted))
	copy(out, rm.submitted)
	return out
}

func itemsFor(t *testing.T, payrollID string) []models.PayrollItem {
	t.Helper()
	var items []models.PayrollItem
	require.NoError(t, models.DB.Where("payroll_id = ?", payrollID).
		Order("id").Find(&items).Error)
	return items
}

// The failed→processing retry edge exists so a transient Monnify outage does not
// dead-letter a batch. But a batch re-entering the worker still carries the
// items Monnify already settled, and re-submitting those pays them a second
// time. This is the regression test for that: on retry, only unsettled items
// may be sent.
func TestProcessPayrollTask_RetryDoesNotResendSettledItems(t *testing.T) {
	skipIfNoDB(t)

	orgID, payrollID := seedPendingBatch(t, 3)
	items := itemsFor(t, payrollID)
	require.Len(t, items, 3)

	// Model the state after a partial run: two items settled by webhook, the
	// batch then marked failed, and Asynq about to retry the task.
	settled := []string{items[0].ID, items[1].ID}
	remaining := items[2].ID
	require.NoError(t, models.DB.Model(&models.PayrollItem{}).
		Where("id IN ?", settled).
		Update("status", models.PayrollCompleted).Error)
	require.NoError(t, models.DB.Model(&models.Payroll{}).
		Where("id = ?", payrollID).
		Updates(map[string]interface{}{"status": models.PayrollFailed, "pending_count": 0}).Error)

	rm := newRecordingMonnify(t)
	t.Setenv("MONNIFY_BASE_URL", rm.server.URL)
	t.Setenv("MOCK_MODE", "false")
	t.Setenv("MONNIFY_API_KEY", "k")
	t.Setenv("MONNIFY_SECRET_KEY", "s")
	t.Setenv("MONNIFY_SOURCE_WALLET", "0123456789")

	payload, err := json.Marshal(map[string]string{"payroll_id": payrollID, "org_id": orgID})
	require.NoError(t, err)

	handler := NewPayrollHandler()
	require.NoError(t, handler.ProcessPayrollTask(
		context.Background(), asynq.NewTask(TypeProcessPayroll, payload)))

	refs := rm.references()
	assert.ElementsMatch(t, []string{remaining}, refs,
		"only the unsettled item may be re-submitted; settled items must never be paid twice")
	for _, id := range settled {
		assert.NotContains(t, refs, id, "already-settled item %s was re-sent to Monnify", id)
	}

	// pending_count must count only items that can still decrement it. Counting
	// the settled ones would leave the batch stuck in processing forever, since
	// their webhooks have already been consumed.
	var payroll models.Payroll
	require.NoError(t, models.DB.First(&payroll, "id = ?", payrollID).Error)
	assert.Equal(t, 1, payroll.PendingCount,
		"pending_count must equal the number of items actually submitted")
	assert.Equal(t, models.PayrollProcessing, payroll.Status)
}

// An item whose employee record is no longer visible can never produce a
// webhook. Left pending it would hold pending_count above zero forever and the
// batch would never reconcile, so the worker must fail it and exclude it from
// the count.
//
// The realistic path is a soft delete, not a hard one: a foreign key stops an
// employee row being removed while payroll items reference it, but GORM's
// default scope silently drops soft-deleted rows from the employee lookup. An
// offboarding between payroll creation and the worker run produces exactly this.
func TestProcessPayrollTask_OrphanedItemIsFailedNotStranded(t *testing.T) {
	skipIfNoDB(t)

	orgID, payrollID := seedPendingBatch(t, 2)
	items := itemsFor(t, payrollID)
	require.Len(t, items, 2)

	orphanItem := items[0]
	require.NoError(t, models.DB.Exec(
		"UPDATE employees SET deleted_at = NOW() WHERE id = ?", orphanItem.EmployeeID).Error)

	rm := newRecordingMonnify(t)
	t.Setenv("MONNIFY_BASE_URL", rm.server.URL)
	t.Setenv("MOCK_MODE", "false")
	t.Setenv("MONNIFY_API_KEY", "k")
	t.Setenv("MONNIFY_SECRET_KEY", "s")
	t.Setenv("MONNIFY_SOURCE_WALLET", "0123456789")

	payload, err := json.Marshal(map[string]string{"payroll_id": payrollID, "org_id": orgID})
	require.NoError(t, err)

	handler := NewPayrollHandler()
	require.NoError(t, handler.ProcessPayrollTask(
		context.Background(), asynq.NewTask(TypeProcessPayroll, payload)))

	var payroll models.Payroll
	require.NoError(t, models.DB.First(&payroll, "id = ?", payrollID).Error)
	assert.Equal(t, 1, payroll.PendingCount,
		"the orphaned item must not be counted — it can never decrement the counter")

	var orphan models.PayrollItem
	require.NoError(t, models.DB.First(&orphan, "id = ?", orphanItem.ID).Error)
	assert.Equal(t, models.PayrollFailed, orphan.Status)
	assert.NotEmpty(t, orphan.ErrorMessage, "a failed item must say why")

	assert.NotContains(t, rm.references(), orphanItem.ID,
		"an item with no employee must not be sent for disbursement")
}

// A batch whose items are all already settled must resolve without calling
// Monnify at all — there is nothing left to pay.
func TestProcessPayrollTask_FullySettledBatchReconcilesWithoutMonnify(t *testing.T) {
	skipIfNoDB(t)

	orgID, payrollID := seedPendingBatch(t, 2)
	items := itemsFor(t, payrollID)

	ids := []string{items[0].ID, items[1].ID}
	require.NoError(t, models.DB.Model(&models.PayrollItem{}).
		Where("id IN ?", ids).
		Update("status", models.PayrollCompleted).Error)
	require.NoError(t, models.DB.Model(&models.Payroll{}).
		Where("id = ?", payrollID).
		Updates(map[string]interface{}{"status": models.PayrollFailed, "pending_count": 0}).Error)

	rm := newRecordingMonnify(t)
	t.Setenv("MONNIFY_BASE_URL", rm.server.URL)
	t.Setenv("MOCK_MODE", "false")
	t.Setenv("MONNIFY_API_KEY", "k")
	t.Setenv("MONNIFY_SECRET_KEY", "s")
	t.Setenv("MONNIFY_SOURCE_WALLET", "0123456789")

	payload, err := json.Marshal(map[string]string{"payroll_id": payrollID, "org_id": orgID})
	require.NoError(t, err)

	handler := NewPayrollHandler()
	require.NoError(t, handler.ProcessPayrollTask(
		context.Background(), asynq.NewTask(TypeProcessPayroll, payload)))

	rm.mu.Lock()
	calls := rm.calls
	rm.mu.Unlock()
	assert.Zero(t, calls, "a fully settled batch must not reach the payment gateway")

	var payroll models.Payroll
	require.NoError(t, models.DB.First(&payroll, "id = ?", payrollID).Error)
	assert.Equal(t, models.PayrollCompleted, payroll.Status,
		"the batch must reconcile rather than sit in processing forever")
}
