//go:build integration

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/internal/workers"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests drive a payroll through its real writers end to end:
// PayrollService.CreatePayroll → the asynq worker → signed Monnify callbacks.
//
// The webhook tests that already exist seed payroll items as 'processing' by
// hand, a state nothing in production ever wrote: the worker handed a batch to
// Monnify and left every item 'pending', so every "paid" callback was an
// illegal pending→completed transition, logged and answered 200. The batch
// stayed 'processing' forever and Monnify stopped retrying. Nothing chained
// the worker into the webhook, so nothing noticed.

func seedActiveSalaried(t *testing.T, orgID, name string, salary money.Kobo) string {
	t.Helper()
	id := "EMP-" + uuid.New().String()[:8]
	require.NoError(t, models.DB.Create(&models.Employee{
		ID:             id,
		OrganizationID: orgID,
		Name:           name,
		Email:          models.EncryptedString(name + "-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		Salary:         salary,
		IsActive:       true,
	}).Error)
	return id
}

// runPayrollThroughWorker creates a payroll via the service and runs the real
// worker handler on it, as the asynq server would.
func runPayrollThroughWorker(t *testing.T, orgID string) (payrollID string, items []models.PayrollItem) {
	t.Helper()
	t.Setenv("MOCK_MODE", "true")
	svc := services.NewPayrollService(
		repository.NewPayrollRepository(models.DB), repository.NewEmployeeRepository(models.DB))
	payroll, err := svc.CreatePayroll(context.Background(), orgID, time.Now().Format(services.PeriodLayout))
	require.NoError(t, err)

	raw, err := json.Marshal(map[string]string{"payroll_id": payroll.ID, "org_id": orgID})
	require.NoError(t, err)
	require.NoError(t, workers.NewPayrollHandler().ProcessPayrollTask(
		context.Background(), asynq.NewTask(workers.TypeProcessPayroll, raw)))

	full, err := svc.GetPayroll(context.Background(), orgID, payroll.ID)
	require.NoError(t, err)
	return payroll.ID, full.Items
}

func deliver(t *testing.T, ref, event string, amount money.Kobo) int {
	t.Helper()
	w, c := signedEWAWebhookRequest(t, ref, event, amount)
	(&WebhookHandler{}).HandleMonnifyWebhook(c)
	return w.Code
}

func reloadPayroll(t *testing.T, payrollID string) models.Payroll {
	t.Helper()
	var p models.Payroll
	require.NoError(t, models.DB.First(&p, "id = ?", payrollID).Error)
	return p
}

func itemStatuses(t *testing.T, payrollID string) map[string]models.PayrollStatus {
	t.Helper()
	var items []models.PayrollItem
	require.NoError(t, models.DB.Where("payroll_id = ?", payrollID).Find(&items).Error)
	out := map[string]models.PayrollStatus{}
	for _, it := range items {
		out[it.EmployeeName] = it.Status
	}
	return out
}

func TestPayroll_WorkerMarksSentItemsProcessing(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	seedActiveSalaried(t, orgID, "Chioma", money.FromNaira(150_000))

	payrollID, items := runPayrollThroughWorker(t, orgID)

	require.Len(t, items, 2)
	for _, it := range items {
		assert.Equal(t, models.PayrollProcessing, it.Status,
			"%s: an item handed to the bank must be 'processing' so its callback can settle it", it.EmployeeName)
	}
	p := reloadPayroll(t, payrollID)
	assert.Equal(t, models.PayrollProcessing, p.Status)
	assert.Equal(t, 2, p.PendingCount)
}

func TestPayroll_AllCallbacksSuccessful_BatchCompletes(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	seedActiveSalaried(t, orgID, "Chioma", money.FromNaira(150_000))
	payrollID, items := runPayrollThroughWorker(t, orgID)

	for _, it := range items {
		require.Equal(t, http.StatusOK, deliver(t, it.ID, "DISBURSEMENT_SUCCESSFUL", it.Amount))
	}

	for name, st := range itemStatuses(t, payrollID) {
		assert.Equal(t, models.PayrollCompleted, st, name)
	}
	p := reloadPayroll(t, payrollID)
	assert.Equal(t, models.PayrollCompleted, p.Status, "batch must close once every item has settled")
	assert.Zero(t, p.PendingCount)
}

func TestPayroll_OneFailedCallback_OthersStillComplete(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	seedActiveSalaried(t, orgID, "Chioma", money.FromNaira(150_000))
	seedActiveSalaried(t, orgID, "Emeka", money.FromNaira(180_000))
	payrollID, items := runPayrollThroughWorker(t, orgID)

	for _, it := range items {
		event := "DISBURSEMENT_SUCCESSFUL"
		if it.EmployeeName == "Emeka" {
			event = "DISBURSEMENT_FAILED"
		}
		require.Equal(t, http.StatusOK, deliver(t, it.ID, event, it.Amount))
	}

	got := itemStatuses(t, payrollID)
	assert.Equal(t, models.PayrollCompleted, got["Musa"])
	assert.Equal(t, models.PayrollCompleted, got["Chioma"])
	assert.Equal(t, models.PayrollFailed, got["Emeka"])
	p := reloadPayroll(t, payrollID)
	assert.Zero(t, p.PendingCount, "every callback must decrement the counter")
	assert.Equal(t, models.PayrollFailed, p.Status, "a batch with a failed line is not 'completed'")
}

// A callback delivered twice, or one that arrives after the item is settled,
// must change nothing.
func TestPayroll_RedeliveredCallbackIsIdempotent(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	seedActiveSalaried(t, orgID, "Chioma", money.FromNaira(150_000))
	payrollID, items := runPayrollThroughWorker(t, orgID)

	first := items[0]
	require.Equal(t, http.StatusOK, deliver(t, first.ID, "DISBURSEMENT_SUCCESSFUL", first.Amount))
	require.Equal(t, http.StatusOK, deliver(t, first.ID, "DISBURSEMENT_SUCCESSFUL", first.Amount))
	require.Equal(t, http.StatusOK, deliver(t, first.ID, "DISBURSEMENT_FAILED", first.Amount),
		"a late contradictory callback is acknowledged but ignored")

	assert.Equal(t, 1, reloadPayroll(t, payrollID).PendingCount,
		"only the first delivery may decrement the counter")
	var reloaded models.PayrollItem
	require.NoError(t, models.DB.First(&reloaded, "id = ?", first.ID).Error)
	assert.Equal(t, models.PayrollCompleted, reloaded.Status)
}
