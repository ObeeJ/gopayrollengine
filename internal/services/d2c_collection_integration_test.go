//go:build integration

package services

import (
	"context"
	"testing"
	"time"

	"go-payroll-engine/internal/integrations/banklink"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedD2CDisbursedAdvance creates a D2C org+employee with one Disbursed
// EWAAdvance and its ledger already posted (Dr advance_receivable / Cr
// cash_settlement) — the exact state a debit collection attempt fires
// against, mirroring seedApprovedAdvance's shape one status further along.
func seedD2CDisbursedAdvance(t *testing.T, amount money.Kobo) (orgID, employeeID, advanceID string) {
	t.Helper()
	orgID, employeeID = seedD2CWorker(t)

	advance := models.EWAAdvance{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		Period:         "d2c-" + uuid.New().String()[:8],
		AmountKobo:     amount,
		Status:         models.AdvanceDisbursed,
		DependencyTier: models.TierHealthy,
	}
	require.NoError(t, models.DB.Create(&advance).Error)

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
				{AccountID: receivable.ID, Direction: models.Debit, Amount: money.NGNFromKobo(amount)},
				{AccountID: cash.ID, Direction: models.Credit, Amount: money.NGNFromKobo(amount)},
			},
		})
		return err
	}))

	return orgID, employeeID, advance.ID
}

// seedD2CMandate links a bank account for employeeID and authorizes a debit
// mandate on it via mock — the state InitiateD2CCollection requires.
func seedD2CMandate(t *testing.T, orgID, employeeID string, mock *banklink.Mock) *models.D2CBankLink {
	t.Helper()
	linked, err := mock.CompleteLink(context.Background(), "callback-token")
	require.NoError(t, err)
	mandateRef, err := mock.AuthorizeDebitMandate(context.Background(), linked.ProviderAccountRef)
	require.NoError(t, err)

	now := time.Now()
	link := models.D2CBankLink{
		OrganizationID:     orgID,
		EmployeeID:         employeeID,
		Provider:           mock.Name(),
		ProviderAccountRef: linked.ProviderAccountRef,
		DebitMandateRef:    &mandateRef,
		DebitAuthorizedAt:  &now,
		LinkedAt:           now,
	}
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Create(&link).Error
	}))
	return &link
}

func TestInitiateD2CCollection_HappyPath(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, advanceID := seedD2CDisbursedAdvance(t, money.FromNaira(10_000))
	mock := banklink.NewMock()
	seedD2CMandate(t, orgID, employeeID, mock)

	collection, err := InitiateD2CCollection(context.Background(), orgID, advanceID, time.Now(), mock)
	require.NoError(t, err)
	assert.Equal(t, 1, collection.AttemptNumber)
	assert.Equal(t, models.D2CCollectionPending, collection.Status)
	require.NotNil(t, collection.ProviderRef)
	assert.Equal(t, money.FromNaira(10_000), collection.AmountKobo)

	debits := mock.Debits()
	require.Len(t, debits, 1)
	assert.Equal(t, money.FromNaira(10_000), money.Kobo(debits[0].Amount.Minor))
	assert.Equal(t, collection.ID, debits[0].Reference)
}

func TestInitiateD2CCollection_RejectsUncollectibleStatus(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CWorker(t)
	mock := banklink.NewMock()
	seedD2CMandate(t, orgID, employeeID, mock)

	advance := models.EWAAdvance{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		Period:         "d2c-req",
		AmountKobo:     money.FromNaira(5_000),
		Status:         models.AdvanceRequested,
		DependencyTier: models.TierHealthy,
	}
	require.NoError(t, models.DB.Create(&advance).Error)

	_, err := InitiateD2CCollection(context.Background(), orgID, advance.ID, time.Now(), mock)
	assert.ErrorIs(t, err, ErrD2CAdvanceNotCollectible)
}

func TestInitiateD2CCollection_RejectsNoLinkedAccount(t *testing.T) {
	skipIfNoDB(t)
	orgID, _, advanceID := seedD2CDisbursedAdvance(t, money.FromNaira(10_000))
	mock := banklink.NewMock()

	_, err := InitiateD2CCollection(context.Background(), orgID, advanceID, time.Now(), mock)
	require.Error(t, err)
}

func TestInitiateD2CCollection_RejectsNoMandate(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, advanceID := seedD2CDisbursedAdvance(t, money.FromNaira(10_000))
	mock := banklink.NewMock()
	linked, err := mock.CompleteLink(context.Background(), "cb")
	require.NoError(t, err)
	// Linked, but never authorized for debit.
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Create(&models.D2CBankLink{
			OrganizationID:     orgID,
			EmployeeID:         employeeID,
			Provider:           mock.Name(),
			ProviderAccountRef: linked.ProviderAccountRef,
			LinkedAt:           time.Now(),
		}).Error
	}))

	_, err = InitiateD2CCollection(context.Background(), orgID, advanceID, time.Now(), mock)
	assert.ErrorIs(t, err, banklink.ErrNoMandate)
}

func TestInitiateD2CCollection_RejectsSecondPendingAttempt(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, advanceID := seedD2CDisbursedAdvance(t, money.FromNaira(10_000))
	mock := banklink.NewMock()
	seedD2CMandate(t, orgID, employeeID, mock)

	_, err := InitiateD2CCollection(context.Background(), orgID, advanceID, time.Now(), mock)
	require.NoError(t, err)

	_, err = InitiateD2CCollection(context.Background(), orgID, advanceID, time.Now(), mock)
	require.Error(t, err, "a second attempt must not be submitted while one is still pending")
}

func TestInitiateD2CCollection_AttemptNumberIncrementsAfterFailure(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, advanceID := seedD2CDisbursedAdvance(t, money.FromNaira(10_000))
	mock := banklink.NewMock()
	seedD2CMandate(t, orgID, employeeID, mock)

	first, err := InitiateD2CCollection(context.Background(), orgID, advanceID, time.Now(), mock)
	require.NoError(t, err)
	assert.Equal(t, 1, first.AttemptNumber)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.ConfirmD2CCollectionFailure(tx, first, "insufficient_funds")
	}))

	second, err := InitiateD2CCollection(context.Background(), orgID, advanceID, time.Now(), mock)
	require.NoError(t, err)
	assert.Equal(t, 2, second.AttemptNumber)
}

func TestConfirmD2CCollectionSuccess_FullyRecoveredSettlesAdvance(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, advanceID := seedD2CDisbursedAdvance(t, money.FromNaira(10_000))
	mock := banklink.NewMock()
	seedD2CMandate(t, orgID, employeeID, mock)

	collection, err := InitiateD2CCollection(context.Background(), orgID, advanceID, time.Now(), mock)
	require.NoError(t, err)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.ConfirmD2CCollectionSuccess(tx, collection)
	}))

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", advanceID).Error)
	assert.Equal(t, models.AdvanceSettled, advance.Status)
	assert.Equal(t, money.FromNaira(10_000), advance.RecoveredKobo)
	require.NotNil(t, advance.SettledAt)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		cash, err := models.EnsureAccount(tx, orgID, "", models.AccountCashSettlement, money.NGN)
		require.NoError(t, err)
		bal, err := models.AccountBalance(tx, cash.ID)
		require.NoError(t, err)
		// Cash was credited (money out) at advance time and debited (money
		// in) at collection time — net zero, the same shape a fully
		// recovered payroll-settled advance leaves behind.
		assert.True(t, bal.IsZero(), "cash_settlement should net back to zero once fully collected")
		return nil
	}))
}

func TestConfirmD2CCollectionSuccess_PartialLeavesAdvanceDisbursed(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, advanceID := seedD2CDisbursedAdvance(t, money.FromNaira(10_000))
	mock := banklink.NewMock()
	seedD2CMandate(t, orgID, employeeID, mock)

	// Only half the outstanding balance actually clears — simulate by
	// hand-crafting a collection row for less than the full amount rather
	// than relying on InitiateD2CCollection, which always targets the full
	// remaining balance.
	collection := &models.D2CDebitCollection{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		AdvanceID:      advanceID,
		AmountKobo:     money.FromNaira(5_000),
		AttemptNumber:  1,
		ScheduledFor:   time.Now(),
		Status:         models.D2CCollectionPending,
		Provider:       mock.Name(),
	}
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Create(collection).Error
	}))

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.ConfirmD2CCollectionSuccess(tx, collection)
	}))

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", advanceID).Error)
	assert.Equal(t, models.AdvanceDisbursed, advance.Status, "partial recovery must not settle the advance")
	assert.Equal(t, money.FromNaira(5_000), advance.RecoveredKobo)
}

func TestConfirmD2CCollectionSuccess_IdempotentOnDoubleConfirm(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, advanceID := seedD2CDisbursedAdvance(t, money.FromNaira(10_000))
	mock := banklink.NewMock()
	seedD2CMandate(t, orgID, employeeID, mock)

	collection, err := InitiateD2CCollection(context.Background(), orgID, advanceID, time.Now(), mock)
	require.NoError(t, err)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.ConfirmD2CCollectionSuccess(tx, collection)
	}))

	err = models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.ConfirmD2CCollectionSuccess(tx, collection)
	})
	assert.ErrorIs(t, err, models.ErrD2CCollectionAlreadyResolved)
}

func TestConfirmD2CCollectionFailure_LeavesDisbursedUnderMaxAttempts(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, advanceID := seedD2CDisbursedAdvance(t, money.FromNaira(10_000))
	mock := banklink.NewMock()
	seedD2CMandate(t, orgID, employeeID, mock)

	collection, err := InitiateD2CCollection(context.Background(), orgID, advanceID, time.Now(), mock)
	require.NoError(t, err)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.ConfirmD2CCollectionFailure(tx, collection, "insufficient_funds")
	}))

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", advanceID).Error)
	assert.Equal(t, models.AdvanceDisbursed, advance.Status, "a single failure must leave room for retry")
}

func TestConfirmD2CCollectionFailure_WritesOffAfterMaxAttempts(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, advanceID := seedD2CDisbursedAdvance(t, money.FromNaira(10_000))
	mock := banklink.NewMock()
	seedD2CMandate(t, orgID, employeeID, mock)

	for i := 0; i < 4; i++ {
		collection, err := InitiateD2CCollection(context.Background(), orgID, advanceID, time.Now(), mock)
		require.NoError(t, err)
		require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
			return models.ConfirmD2CCollectionFailure(tx, collection, "insufficient_funds")
		}))
	}

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", advanceID).Error)
	assert.Equal(t, models.AdvanceWrittenOff, advance.Status,
		"the advance must be written off once collection attempts are exhausted")
}

// findD2CSweepResult picks out one advance's own result from a sweep that
// also processes every other D2C org's advances left behind by other tests
// sharing this database — a sweep is meant to see the whole table, so
// asserting on the full result slice's length would be testing test
// pollution, not this function's behavior.
func findD2CSweepResult(results []D2CSweepResult, advanceID string) *D2CSweepResult {
	for i := range results {
		if results[i].AdvanceID == advanceID {
			return &results[i]
		}
	}
	return nil
}

func TestSweepD2CCollections_InitiatesWhenPaydayDue(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, advanceID := seedD2CDisbursedAdvance(t, money.FromNaira(50_000))
	mock := banklink.NewMock()
	link := seedD2CMandate(t, orgID, employeeID, mock)

	// Last paycheck landed exactly 30 days ago on a 30-day cadence — the
	// next one, per that pattern, is due exactly today.
	mock.Transactions[link.ProviderAccountRef] = recurringCredits(4, 30, 30, money.FromNaira(300_000))

	results, err := SweepD2CCollections(context.Background(), predictionNow, mock)
	require.NoError(t, err)
	result := findD2CSweepResult(results, advanceID)
	require.NotNil(t, result, "this advance must appear in the sweep's results")
	require.NoError(t, result.Err)
	require.NotNil(t, result.Collection)
}

func TestSweepD2CCollections_SkipsWhenNotDueYet(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, advanceID := seedD2CDisbursedAdvance(t, money.FromNaira(50_000))
	mock := banklink.NewMock()
	link := seedD2CMandate(t, orgID, employeeID, mock)

	// Paycheck last landed 5 days ago on a monthly cadence — next one is
	// weeks away, not due today.
	mock.Transactions[link.ProviderAccountRef] = recurringCredits(4, 30, 5, money.FromNaira(300_000))

	results, err := SweepD2CCollections(context.Background(), predictionNow, mock)
	require.NoError(t, err)
	result := findD2CSweepResult(results, advanceID)
	require.NotNil(t, result, "this advance must appear in the sweep's results")
	assert.NoError(t, result.Err)
	assert.Nil(t, result.Collection, "nothing should be collected before the predicted payday arrives")
}

func TestSweepD2CCollections_SkipsAdvanceAlreadyInFlight(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, advanceID := seedD2CDisbursedAdvance(t, money.FromNaira(50_000))
	mock := banklink.NewMock()
	link := seedD2CMandate(t, orgID, employeeID, mock)
	mock.Transactions[link.ProviderAccountRef] = recurringCredits(4, 30, 30, money.FromNaira(300_000))

	_, err := InitiateD2CCollection(context.Background(), orgID, advanceID, predictionNow, mock)
	require.NoError(t, err)

	results, err := SweepD2CCollections(context.Background(), predictionNow, mock)
	require.NoError(t, err)
	result := findD2CSweepResult(results, advanceID)
	assert.Nil(t, result, "an advance with a pending attempt must not be swept again")
}
