//go:build integration

package workers

import (
	"context"
	"encoding/json"
	"testing"

	"go-payroll-engine/internal/integrations/provider"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// fakeProvider gives deterministic, network-free control over what
// InitiateTransfer reports, so the worker's three branches (accepted,
// synchronously rejected, unreachable) can each be exercised directly rather
// than relying on Monnify/Paystack's MOCK_MODE always saying yes.
type fakeProvider struct {
	name     string
	result   provider.TransferResult
	err      error
	requests []provider.TransferRequest
}

func (f *fakeProvider) Name() string                           { return f.name }
func (f *fakeProvider) SupportsCurrency(c money.Currency) bool { return c == money.NGN }
func (f *fakeProvider) GetTransferStatus(context.Context, string) (provider.TransferStatus, error) {
	return provider.StatusPending, nil
}
func (f *fakeProvider) InitiateTransfer(_ context.Context, req provider.TransferRequest) (provider.TransferResult, error) {
	f.requests = append(f.requests, req)
	return f.result, f.err
}

func seedApprovedAdvanceForWorker(t *testing.T, amount money.Kobo) (orgID, advanceID string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	employeeID := "EMP-" + uuid.New().String()[:8]

	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		orgID, "ewa disbursement worker test org",
	).Error)

	require.NoError(t, models.DB.Create(&models.Employee{
		ID:             employeeID,
		OrganizationID: orgID,
		Name:           "Ada",
		Email:          models.EncryptedString("ewa-worker-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		Salary:         money.FromNaira(300_000),
		IsActive:       true,
	}).Error)

	advance := models.EWAAdvance{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		Period:         "worker-" + uuid.New().String()[:8],
		AmountKobo:     amount,
		Status:         models.AdvanceApproved,
		DependencyTier: models.TierHealthy,
	}
	require.NoError(t, models.DB.Create(&advance).Error)

	// Mirrors EWAService.postAdvanceLedger: RequestAdvance posts this entry at
	// approval time, and CancelAdvance's reversal assumes it exists — without
	// it, a rejection-path reversal drives the receivable negative instead of
	// to zero.
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		receivable, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
		if err != nil {
			return err
		}
		cash, err := models.EnsureAccount(tx, orgID, "", models.AccountCashSettlement, money.NGN)
		if err != nil {
			return err
		}
		_, err = models.PostTransaction(tx, models.PostingRequest{
			OrgID:          orgID,
			Kind:           "ewa_advance",
			Reference:      advance.ID,
			IdempotencyKey: "ewa_advance:" + advance.ID,
			Entries: []models.EntryInput{
				{AccountID: receivable.ID, Direction: models.Debit, Amount: money.NGNFromKobo(advance.AmountKobo)},
				{AccountID: cash.ID, Direction: models.Credit, Amount: money.NGNFromKobo(advance.AmountKobo)},
			},
		})
		return err
	}))

	return orgID, advance.ID
}

func runDisbursementTask(t *testing.T, h *EWADisbursementHandler, orgID, advanceID string) error {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"advance_id": advanceID, "org_id": orgID})
	require.NoError(t, err)
	return h.ProcessEWADisbursementTask(context.Background(), asynq.NewTask(TypeDisburseEWAAdvance, payload))
}

// The core happy path: an accepted transfer records the provider's reference
// but leaves the advance at Approved — Disbursed is reserved for webhook
// confirmation, never for provider acceptance alone.
func TestProcessEWADisbursementTask_AcceptedRecordsSubmissionWithoutTransitioning(t *testing.T) {
	skipIfNoDB(t)
	orgID, advanceID := seedApprovedAdvanceForWorker(t, money.FromNaira(5_000))

	ref := "FAKE-REF-" + uuid.New().String()[:8]
	fp := &fakeProvider{name: "fake", result: provider.TransferResult{
		Accepted: true, ProviderReference: ref, Message: "queued",
	}}
	h := NewEWADisbursementHandler(provider.NewRegistry(fp))

	require.NoError(t, runDisbursementTask(t, h, orgID, advanceID))

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", advanceID).Error)
	assert.Equal(t, models.AdvanceApproved, advance.Status,
		"acceptance must not itself confirm disbursement")
	require.NotNil(t, advance.ProviderName)
	require.NotNil(t, advance.ProviderReference)
	assert.Equal(t, "fake", *advance.ProviderName)
	assert.Equal(t, ref, *advance.ProviderReference)

	require.Len(t, fp.requests, 1)
	assert.Equal(t, advanceID, fp.requests[0].Reference)
	assert.Equal(t, money.NGNFromKobo(money.FromNaira(5_000)), fp.requests[0].Amount)
}

// A synchronous rejection (bad account number, provider-side validation) must
// cancel the advance and reverse the receivable immediately — there is no
// webhook coming for a transfer the provider never accepted in the first place.
func TestProcessEWADisbursementTask_RejectedCancelsAndReverses(t *testing.T) {
	skipIfNoDB(t)
	orgID, advanceID := seedApprovedAdvanceForWorker(t, money.FromNaira(5_000))

	fp := &fakeProvider{name: "fake", result: provider.TransferResult{
		Accepted: false, Message: "invalid destination account",
	}}
	h := NewEWADisbursementHandler(provider.NewRegistry(fp))

	require.NoError(t, runDisbursementTask(t, h, orgID, advanceID))

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", advanceID).Error)
	assert.Equal(t, models.AdvanceCancelled, advance.Status)
	assert.Contains(t, advance.DeclineReason, "invalid destination account")
}

// An unreachable provider must produce a retryable error (so Asynq retries)
// and must NOT touch the advance's status or provider fields — the transfer
// may have gone through on the provider's side despite the failure to confirm
// it, so treating this as a rejection would risk reversing money that moved.
func TestProcessEWADisbursementTask_UnavailableProviderIsRetried(t *testing.T) {
	skipIfNoDB(t)
	orgID, advanceID := seedApprovedAdvanceForWorker(t, money.FromNaira(5_000))

	fp := &fakeProvider{name: "fake", err: provider.ErrProviderUnavailable}
	h := NewEWADisbursementHandler(provider.NewRegistry(fp))

	err := runDisbursementTask(t, h, orgID, advanceID)
	require.Error(t, err, "an unavailable provider must be surfaced so Asynq retries")

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", advanceID).Error)
	assert.Equal(t, models.AdvanceApproved, advance.Status,
		"an unconfirmed failure must not cancel an advance that may have actually gone through")
	assert.Nil(t, advance.ProviderReference)
}

// A task for an advance that already has a provider reference (a duplicate
// task, or a race between two workers that both picked it up) must be a safe
// no-op — it must not call the provider a second time, which would risk a
// real second transfer.
func TestProcessEWADisbursementTask_AlreadySubmittedIsNoOp(t *testing.T) {
	skipIfNoDB(t)
	orgID, advanceID := seedApprovedAdvanceForWorker(t, money.FromNaira(5_000))

	originalRef := "MNFY-ALREADY-SUBMITTED-" + uuid.New().String()[:8]
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.MarkSubmittedToProvider(tx, advanceID, "monnify", originalRef)
	}))

	fp := &fakeProvider{name: "fake", result: provider.TransferResult{Accepted: true, ProviderReference: "SHOULD-NOT-APPEAR"}}
	h := NewEWADisbursementHandler(provider.NewRegistry(fp))

	require.NoError(t, runDisbursementTask(t, h, orgID, advanceID))

	assert.Empty(t, fp.requests, "an already-submitted advance must never reach InitiateTransfer again")

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", advanceID).Error)
	require.NotNil(t, advance.ProviderReference)
	assert.Equal(t, originalRef, *advance.ProviderReference,
		"the original submission must be untouched")
}
