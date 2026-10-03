//go:build integration

package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/internal/testutil"
	"go-payroll-engine/internal/workers"
	"go-payroll-engine/pkg/money"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// recordingBank is a fake Monnify that remembers every reference it was asked
// to pay, so a test can assert exactly what went to the bank on each run.
type recordingBank struct {
	srv  *httptest.Server
	mu   sync.Mutex
	sent [][]string // references per bulk call
}

func newRecordingBank(t *testing.T) *recordingBank {
	t.Helper()
	b := &recordingBank{}
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
		refs := make([]string, 0, len(body.TransactionList))
		for _, l := range body.TransactionList {
			refs = append(refs, l.Reference)
		}
		b.mu.Lock()
		b.sent = append(b.sent, refs)
		b.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"requestSuccessful": true, "responseMessage": "ok",
			"responseBody": map[string]interface{}{"batchReference": body.BatchReference, "status": "SUCCESSFUL"},
		})
	})
	b.srv = httptest.NewServer(mux)
	t.Cleanup(b.srv.Close)
	t.Setenv("MOCK_MODE", "false")
	t.Setenv("MONNIFY_BASE_URL", b.srv.URL)
	t.Setenv("MONNIFY_API_KEY", "k")
	t.Setenv("MONNIFY_SECRET_KEY", "test-webhook-secret")
	t.Setenv("MONNIFY_SOURCE_WALLET", "9999999999")
	return b
}

func (b *recordingBank) calls() [][]string {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([][]string, len(b.sent))
	copy(out, b.sent)
	return out
}

func runWorker(t *testing.T, orgID, payrollID string) {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"payroll_id": payrollID, "org_id": orgID})
	require.NoError(t, err)
	h := &workers.PayrollHandler{MonnifyClient: monnify.NewClient()}
	require.NoError(t, h.ProcessPayrollTask(context.Background(), asynq.NewTask(workers.TypeProcessPayroll, raw)))
}

func newPayrollSvc() *services.PayrollService {
	return services.NewPayrollService(
		repository.NewPayrollRepository(models.DB), repository.NewEmployeeRepository(models.DB))
}

func createPayroll(t *testing.T, orgID string) string {
	t.Helper()
	p, err := newPayrollSvc().CreatePayroll(context.Background(), orgID, time.Now().Format(services.PeriodLayout))
	require.NoError(t, err)
	return p.ID
}

func itemByName(t *testing.T, payrollID, name string) models.PayrollItem {
	t.Helper()
	var it models.PayrollItem
	require.NoError(t, models.DB.Where("payroll_id = ? AND employee_name = ?", payrollID, name).First(&it).Error)
	return it
}

// partiallyPaidBatch: Musa and Chioma paid, Emeka's transfer bounced. Returns
// the batch and Emeka's item.
func partiallyPaidBatch(t *testing.T, orgID string) (payrollID string, emeka models.PayrollItem) {
	t.Helper()
	seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	seedActiveSalaried(t, orgID, "Chioma", money.FromNaira(150_000))
	seedActiveSalaried(t, orgID, "Emeka", money.FromNaira(180_000))
	payrollID = createPayroll(t, orgID)
	runWorker(t, orgID, payrollID)
	for _, name := range []string{"Musa", "Chioma"} {
		it := itemByName(t, payrollID, name)
		require.Equal(t, http.StatusOK, deliver(t, it.ItemReference(), "DISBURSEMENT_SUCCESSFUL", it.Amount))
	}
	emeka = itemByName(t, payrollID, "Emeka")
	require.Equal(t, http.StatusOK, deliver(t, emeka.ItemReference(), "DISBURSEMENT_FAILED", emeka.Amount))
	require.Equal(t, models.PayrollFailed, reloadPayroll(t, payrollID).Status)
	return payrollID, emeka
}

// ---- what the item remembers ----------------------------------------------

func TestPayroll_ItemsRecordTheirBreakdownAndWhenTheyWereSent(t *testing.T) {
	skipIfNoDB(t)
	bank := newRecordingBank(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	emp := seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	require.NoError(t, models.DB.Create(&models.EWASavingsPreference{
		OrganizationID: orgID, EmployeeID: emp, Enabled: true, Mode: models.SavingsFixedPercent, FixedPercent: 5,
	}).Error)

	payrollID := createPayroll(t, orgID)
	it := itemByName(t, payrollID, "Musa")
	require.NotNil(t, it.GrossKobo)
	require.NotNil(t, it.SavingsKobo)
	require.NotNil(t, it.AdvancesDeductedKobo)
	assert.Equal(t, money.FromNaira(300_000), *it.GrossKobo)
	assert.Equal(t, money.FromNaira(15_000), *it.SavingsKobo, "5% of ₦300,000")
	assert.Zero(t, *it.AdvancesDeductedKobo)
	assert.Equal(t, money.FromNaira(285_000), it.Amount, "net = gross − advances − savings")
	assert.Nil(t, it.SentAt, "not sent yet")

	runWorker(t, orgID, payrollID)
	it = itemByName(t, payrollID, "Musa")
	require.NotNil(t, it.SentAt, "the moment it went to the bank is what stuck-item detection measures from")
	assert.Equal(t, [][]string{{it.ID}}, bank.calls(), "attempt 1 is sent under the bare item ID")
}

// ---- retry ------------------------------------------------------------------

func TestRetryFailedItems_ResendsOnlyTheFailedLineUnderANewReference(t *testing.T) {
	skipIfNoDB(t)
	bank := newRecordingBank(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, emeka := partiallyPaidBatch(t, orgID)
	require.Len(t, bank.calls(), 1)

	res, err := newPayrollSvc().RetryFailedItems(context.Background(), orgID, payrollID, services.Actor{Name: "admin", IP: "127.0.0.1"})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Retrying)
	assert.Empty(t, res.Skipped)

	after := itemByName(t, payrollID, "Emeka")
	assert.Equal(t, models.PayrollPending, after.Status)
	assert.Equal(t, 2, after.Attempt)
	assert.Empty(t, after.ErrorMessage)
	assert.Equal(t, models.PayrollFailed, reloadPayroll(t, payrollID).Status, "the batch stays failed until the retry is picked up")

	runWorker(t, orgID, payrollID)
	calls := bank.calls()
	require.Len(t, calls, 2)
	assert.Equal(t, []string{emeka.ID + "-R2"}, calls[1],
		"only Emeka, and under a new reference — a provider may answer a re-used failed reference with the old failure; Musa and Chioma must never be re-sent")
	p := reloadPayroll(t, payrollID)
	assert.Equal(t, models.PayrollProcessing, p.Status)
	assert.Equal(t, 1, p.PendingCount)
}

// A late callback from the first attempt must not touch the retry in flight.
func TestRetryFailedItems_StaleCallbackFromTheOldAttemptIsIgnored(t *testing.T) {
	skipIfNoDB(t)
	newRecordingBank(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, emeka := partiallyPaidBatch(t, orgID)
	_, err := newPayrollSvc().RetryFailedItems(context.Background(), orgID, payrollID, services.Actor{Name: "admin"})
	require.NoError(t, err)
	runWorker(t, orgID, payrollID)

	// Attempt 1 redelivers its failure, and (contradictorily) a success.
	require.Equal(t, http.StatusOK, deliver(t, emeka.ID, "DISBURSEMENT_FAILED", emeka.Amount))
	require.Equal(t, http.StatusOK, deliver(t, emeka.ID, "DISBURSEMENT_SUCCESSFUL", emeka.Amount))
	assert.Equal(t, models.PayrollProcessing, itemByName(t, payrollID, "Emeka").Status,
		"callbacks for a superseded attempt change nothing")
	assert.Equal(t, 1, reloadPayroll(t, payrollID).PendingCount)

	// Attempt 2's own callback settles it and closes the batch.
	require.Equal(t, http.StatusOK, deliver(t, emeka.ID+"-R2", "DISBURSEMENT_SUCCESSFUL", emeka.Amount))
	assert.Equal(t, models.PayrollCompleted, itemByName(t, payrollID, "Emeka").Status)
	p := reloadPayroll(t, payrollID)
	assert.Equal(t, models.PayrollCompleted, p.Status, "with nothing left failed, the batch is paid")
	assert.Zero(t, p.PendingCount)
}

func TestRetryFailedItems_RetriedLineFailingAgainReopensTheFailure(t *testing.T) {
	skipIfNoDB(t)
	newRecordingBank(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, emeka := partiallyPaidBatch(t, orgID)
	svc := newPayrollSvc()
	_, err := svc.RetryFailedItems(context.Background(), orgID, payrollID, services.Actor{Name: "admin"})
	require.NoError(t, err)
	runWorker(t, orgID, payrollID)
	require.Equal(t, http.StatusOK, deliver(t, emeka.ID+"-R2", "DISBURSEMENT_FAILED", emeka.Amount))
	assert.Equal(t, models.PayrollFailed, reloadPayroll(t, payrollID).Status)

	// ...and it can be retried again, as attempt 3.
	_, err = svc.RetryFailedItems(context.Background(), orgID, payrollID, services.Actor{Name: "admin"})
	require.NoError(t, err)
	assert.Equal(t, 3, itemByName(t, payrollID, "Emeka").Attempt)
}

func TestRetryFailedItems_Preconditions(t *testing.T) {
	skipIfNoDB(t)
	newRecordingBank(t)
	svc := newPayrollSvc()
	actor := services.Actor{Name: "admin"}

	t.Run("a batch still processing cannot be retried", func(t *testing.T) {
		orgID := seedOrgWithCurrency(t, money.NGN)
		seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
		payrollID := createPayroll(t, orgID)
		runWorker(t, orgID, payrollID)
		_, err := svc.RetryFailedItems(context.Background(), orgID, payrollID, actor)
		assert.ErrorIs(t, err, services.ErrPayrollNotRetryable)
	})

	t.Run("a fully paid batch cannot be retried", func(t *testing.T) {
		orgID := seedOrgWithCurrency(t, money.NGN)
		seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
		payrollID := createPayroll(t, orgID)
		runWorker(t, orgID, payrollID)
		it := itemByName(t, payrollID, "Musa")
		require.Equal(t, http.StatusOK, deliver(t, it.ID, "DISBURSEMENT_SUCCESSFUL", it.Amount))
		_, err := svc.RetryFailedItems(context.Background(), orgID, payrollID, actor)
		assert.ErrorIs(t, err, services.ErrPayrollNotRetryable)
	})

	t.Run("a terminated employee's line is skipped, not paid", func(t *testing.T) {
		orgID := seedOrgWithCurrency(t, money.NGN)
		payrollID, emeka := partiallyPaidBatch(t, orgID)
		require.NoError(t, models.DB.Model(&models.Employee{}).Where("id = ?", emeka.EmployeeID).Update("is_active", false).Error)
		res, err := svc.RetryFailedItems(context.Background(), orgID, payrollID, actor)
		assert.ErrorIs(t, err, services.ErrNothingToRetry)
		require.NotNil(t, res)
		require.Len(t, res.Skipped, 1)
		assert.Equal(t, emeka.EmployeeID, res.Skipped[0].EmployeeID)
		assert.Equal(t, models.PayrollFailed, itemByName(t, payrollID, "Emeka").Status, "left failed")
	})

	t.Run("another organisation cannot see it", func(t *testing.T) {
		orgID := seedOrgWithCurrency(t, money.NGN)
		payrollID, _ := partiallyPaidBatch(t, orgID)
		other := seedOrgWithCurrency(t, money.NGN)
		testutil.UseAppRoleDB(t)
		_, err := svc.RetryFailedItems(context.Background(), other, payrollID, actor)
		assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	})
}

// ---- outcome and history ------------------------------------------------------

func TestGetPayrollView_PartialPaymentReadsAsPartiallyPaid(t *testing.T) {
	skipIfNoDB(t)
	newRecordingBank(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, emeka := partiallyPaidBatch(t, orgID)

	v, err := newPayrollSvc().GetPayrollView(context.Background(), orgID, payrollID)
	require.NoError(t, err)
	assert.Equal(t, models.OutcomePartiallyPaid, v.Summary.Outcome)
	assert.Equal(t, 2, v.Summary.Paid)
	assert.Equal(t, 1, v.Summary.Failed)
	assert.Equal(t, emeka.Amount, v.Summary.FailedKobo)
	assert.Len(t, v.Items, 3)
}

func TestListPayrolls_NewestFirstPaginatedAndTenantScoped(t *testing.T) {
	skipIfNoDB(t)
	newRecordingBank(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	svc := newPayrollSvc()
	ctx := context.Background()
	var ids []string
	for _, period := range []string{"2026-05", "2026-06", "2026-07"} {
		p, err := svc.CreatePayroll(ctx, orgID, period)
		require.NoError(t, err)
		ids = append(ids, p.ID)
		time.Sleep(10 * time.Millisecond) // distinct created_at
	}
	other := seedOrgWithCurrency(t, money.NGN)
	seedActiveSalaried(t, other, "Other", money.FromNaira(100_000))
	_, err := svc.CreatePayroll(ctx, other, "2026-07")
	require.NoError(t, err)

	got, total, err := svc.ListPayrolls(ctx, orgID, 1, 2)
	require.NoError(t, err)
	assert.EqualValues(t, 3, total, "only this org's batches")
	require.Len(t, got, 2)
	assert.Equal(t, ids[2], got[0].ID, "newest first")
	assert.Equal(t, ids[1], got[1].ID)
	assert.Equal(t, models.OutcomePending, got[0].Summary.Outcome)
	assert.Equal(t, 1, got[0].Summary.Pending)

	page2, _, err := svc.ListPayrolls(ctx, orgID, 2, 2)
	require.NoError(t, err)
	require.Len(t, page2, 1)
	assert.Equal(t, ids[0], page2[0].ID)

	t.Run("production role (RLS)", func(t *testing.T) {
		testutil.UseAppRoleDB(t)
		got, total, err := svc.ListPayrolls(ctx, orgID, 1, 50)
		require.NoError(t, err)
		assert.EqualValues(t, 3, total)
		assert.Len(t, got, 3)
	})
}

// ---- manual resolution of a payout whose callback never came -------------------

func stuckItem(t *testing.T, orgID string) (payrollID string, it models.PayrollItem) {
	t.Helper()
	newRecordingBank(t)
	seedActiveSalaried(t, orgID, "Musa", money.FromNaira(300_000))
	payrollID = createPayroll(t, orgID)
	runWorker(t, orgID, payrollID)
	it = itemByName(t, payrollID, "Musa")
	require.Equal(t, models.PayrollProcessing, it.Status)
	return payrollID, it
}

func backdateSent(t *testing.T, itemID string, ago time.Duration) {
	t.Helper()
	require.NoError(t, models.DB.Model(&models.PayrollItem{}).Where("id = ?", itemID).
		Update("sent_at", time.Now().Add(-ago)).Error)
}

func TestResolveItem_OnlyOnceCallbacksAreOverdue(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, it := stuckItem(t, orgID)

	_, err := newPayrollSvc().ResolveItem(context.Background(), orgID, payrollID, it.ID, services.ResolvePaid,
		"saw it settled on the Monnify dashboard", "MNFY-123", services.Actor{Name: "admin"})
	assert.ErrorIs(t, err, services.ErrNotYetStuck,
		"a callback may still be in flight; an admin must not be able to mark a payout paid on a hunch")
	assert.Equal(t, models.PayrollProcessing, itemByName(t, payrollID, "Musa").Status)
}

func TestResolveItem_PaidClosesTheBatchAndRecordsTheAttestation(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, it := stuckItem(t, orgID)
	backdateSent(t, it.ID, models.StuckAfter+time.Minute)

	got, err := newPayrollSvc().ResolveItem(context.Background(), orgID, payrollID, it.ID, services.ResolvePaid,
		"saw it settled on the Monnify dashboard", "MNFY-123", services.Actor{Name: "admin", IP: "10.0.0.9"})
	require.NoError(t, err)
	assert.Equal(t, models.PayrollCompleted, got.Status)
	require.NotNil(t, got.SettledAt)
	require.NotNil(t, got.ResolvedBy)
	assert.Equal(t, "admin", *got.ResolvedBy)
	assert.Equal(t, "saw it settled on the Monnify dashboard", *got.ResolutionNote)
	assert.Equal(t, "MNFY-123", *got.ResolutionEvidence)

	p := reloadPayroll(t, payrollID)
	assert.Equal(t, models.PayrollCompleted, p.Status, "the same closing logic as a callback")
	assert.Zero(t, p.PendingCount)

	var audits int64
	require.NoError(t, models.DB.Model(&models.AuditEvent{}).
		Where("organization_id = ? AND entity_id = ? AND action = ?", orgID, it.ID, "manually_resolved").
		Count(&audits).Error)
	assert.EqualValues(t, 1, audits)

	// A callback that finally turns up changes nothing.
	require.Equal(t, http.StatusOK, deliver(t, it.ID, "DISBURSEMENT_FAILED", it.Amount))
	assert.Equal(t, models.PayrollCompleted, itemByName(t, payrollID, "Musa").Status)
}

func TestResolveItem_FailedMakesItRetryable(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, it := stuckItem(t, orgID)
	backdateSent(t, it.ID, models.StuckAfter+time.Minute)
	svc := newPayrollSvc()

	_, err := svc.ResolveItem(context.Background(), orgID, payrollID, it.ID, services.ResolveFailed,
		"Monnify shows no such transfer", "MNFY-STATEMENT-2026-10-03 (no transfer for this reference)", services.Actor{Name: "admin"})
	require.NoError(t, err)
	assert.Equal(t, models.PayrollFailed, reloadPayroll(t, payrollID).Status)

	res, err := svc.RetryFailedItems(context.Background(), orgID, payrollID, services.Actor{Name: "admin"})
	require.NoError(t, err)
	assert.Equal(t, 1, res.Retrying)
}

// Marking a payment failed is what makes it retryable, and retrying a payment
// that actually succeeded pays the worker twice. Of the two attestations, this
// is the one that can cost real money, so it must carry evidence.
func TestResolveItem_FailedWithoutEvidenceIsRefused(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, it := stuckItem(t, orgID)
	backdateSent(t, it.ID, models.StuckAfter+time.Minute)

	_, err := newPayrollSvc().ResolveItem(context.Background(), orgID, payrollID, it.ID, services.ResolveFailed,
		"it looks like it failed", "", services.Actor{Name: "admin"})
	require.ErrorIs(t, err, services.ErrInvalidResolution)
	assert.Contains(t, err.Error(), "twice", "the refusal must say why: a retry of a payment that succeeded pays twice")
	assert.Equal(t, models.PayrollProcessing, itemByName(t, payrollID, "Musa").Status, "nothing changed")
}

func TestResolveItem_Validation(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, it := stuckItem(t, orgID)
	backdateSent(t, it.ID, models.StuckAfter+time.Minute)
	svc := newPayrollSvc()
	actor := services.Actor{Name: "admin"}
	ctx := context.Background()

	_, err := svc.ResolveItem(ctx, orgID, payrollID, it.ID, services.ResolvePaid, "", "", actor)
	assert.ErrorIs(t, err, services.ErrInvalidResolution, "an attestation needs a reason")
	_, err = svc.ResolveItem(ctx, orgID, payrollID, it.ID, services.ResolvePaid, "  ", "", actor)
	assert.ErrorIs(t, err, services.ErrInvalidResolution)
	_, err = svc.ResolveItem(ctx, orgID, payrollID, it.ID, services.ResolveOutcome("maybe"), "reason given", "", actor)
	assert.ErrorIs(t, err, services.ErrInvalidResolution)

	_, err = svc.ResolveItem(ctx, orgID, payrollID, "ITEM-nosuch", services.ResolvePaid, "reason given", "", actor)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)

	// Once settled it is no longer resolvable.
	_, err = svc.ResolveItem(ctx, orgID, payrollID, it.ID, services.ResolvePaid, "settled per dashboard", "", actor)
	require.NoError(t, err)
	_, err = svc.ResolveItem(ctx, orgID, payrollID, it.ID, services.ResolveFailed, "changed my mind", "statement", actor)
	assert.ErrorIs(t, err, services.ErrItemNotResolvable)
}

func TestResolveItem_OtherTenantGetsNotFound(t *testing.T) {
	skipIfNoDB(t)
	orgID := seedOrgWithCurrency(t, money.NGN)
	payrollID, it := stuckItem(t, orgID)
	backdateSent(t, it.ID, models.StuckAfter+time.Minute)
	other := seedOrgWithCurrency(t, money.NGN)

	testutil.UseAppRoleDB(t)
	_, err := newPayrollSvc().ResolveItem(context.Background(), other, payrollID, it.ID, services.ResolvePaid,
		"hostile attempt", "", services.Actor{Name: "admin"})
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
}
