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

// seedD2CLinkedWorker creates a D2C org + employee (via seedD2CWorker) with
// an already-Linked D2CBankLink and an active ConsentTypeD2CBankLinkRead
// consent — the same two side effects a real D2CBankLinkHandler.CompleteLink
// call leaves behind, and the state eligibilityTx's D2C branch expects to
// find. Both are required: d2cIncomeBasisTx checks the consent via
// models.HasActiveConsent independently of the link's own Status.
func seedD2CLinkedWorker(t *testing.T) (orgID, employeeID, accountRef string) {
	t.Helper()
	orgID, employeeID = seedD2CWorker(t)
	accountRef = "mock-acct-" + uuid.New().String()[:8]

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		if err := tx.Create(&models.D2CBankLink{
			OrganizationID:     orgID,
			EmployeeID:         employeeID,
			Provider:           "mock",
			ProviderAccountRef: accountRef,
			LinkedAt:           time.Now(),
		}).Error; err != nil {
			return err
		}
		return tx.Create(&models.ConsentRecord{
			OrganizationID: orgID,
			EmployeeID:     employeeID,
			ConsentType:    models.ConsentTypeD2CBankLinkRead,
			Granted:        true,
			ConsentedAt:    time.Now(),
		}).Error
	}))
	return orgID, employeeID, accountRef
}

func TestGetEligibility_D2CWorker_UsesPredictedIncomeMidPeriod(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, accountRef := seedD2CLinkedWorker(t)

	mock := banklink.NewMock()
	// Last occurrence 15 days before predictionNow, 30-day cadence — next
	// predicted payday lands 15 days after predictionNow, so predictionNow
	// sits exactly halfway through the predicted pay cycle.
	mock.Transactions[accountRef] = recurringCredits(4, 30, 15, money.FromNaira(300_000))
	svc := &EWAService{D2CProvider: mock}

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, predictionNow)
	require.NoError(t, err)
	require.False(t, el.Blocked, "blocked_reason=%s", el.BlockedReason)
	assert.Equal(t, money.FromNaira(300_000), el.MonthlySalary)
	// Halfway through a 30-day cycle: ~150,000 kobo-equivalent accrued.
	// Percent uses integer division, so allow the exact expected split.
	expected, err := money.FromNaira(300_000).Percent(15, 30)
	require.NoError(t, err)
	assert.Equal(t, expected, el.AccruedToDate)
}

func TestGetEligibility_D2CWorker_FullyAccruedAtPredictedPayday(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, accountRef := seedD2CLinkedWorker(t)

	mock := banklink.NewMock()
	// Last occurrence 30 days before predictionNow — next predicted payday
	// lands exactly on predictionNow, so it's fully accrued.
	mock.Transactions[accountRef] = recurringCredits(4, 30, 30, money.FromNaira(300_000))
	svc := &EWAService{D2CProvider: mock}

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, predictionNow)
	require.NoError(t, err)
	require.False(t, el.Blocked, "blocked_reason=%s", el.BlockedReason)
	assert.Equal(t, money.FromNaira(300_000), el.AccruedToDate)
}

func TestGetEligibility_D2CWorker_NoLinkedAccountBlocks(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CWorker(t)
	svc := &EWAService{D2CProvider: banklink.NewMock()}

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, predictionNow)
	require.NoError(t, err)
	assert.True(t, el.Blocked)
	assert.Equal(t, DeclineNoIncomeHistory, el.BlockedReason)
}

// The link itself is still Status=Linked — nothing here touches
// D2CBankLink — but a later ConsentRecord withdrew read consent (the same
// row D2CBankLinkHandler.Revoke or the employer-facing
// ConsentHandler.RecordConsent would write). This must block exactly like
// having no linked account at all: the whole point of checking
// HasActiveConsent independently of the link's own Status is that either
// kind of withdrawal has to take effect on its own, not only when
// something also flips the link row.
func TestGetEligibility_D2CWorker_WithdrawnReadConsentBlocksDespiteLinkedStatus(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, accountRef := seedD2CLinkedWorker(t)

	mock := banklink.NewMock()
	mock.Transactions[accountRef] = recurringCredits(4, 30, 30, money.FromNaira(300_000))
	svc := &EWAService{D2CProvider: mock}

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Create(&models.ConsentRecord{
			OrganizationID: orgID,
			EmployeeID:     employeeID,
			ConsentType:    models.ConsentTypeD2CBankLinkRead,
			Granted:        false,
			ConsentedAt:    time.Now().Add(time.Second),
		}).Error
	}))

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, predictionNow)
	require.NoError(t, err)
	assert.True(t, el.Blocked)
	assert.Equal(t, DeclineNoIncomeHistory, el.BlockedReason)
}

func TestGetEligibility_D2CWorker_NoProviderConfiguredBlocks(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, _ := seedD2CLinkedWorker(t)
	svc := &EWAService{} // D2CProvider left nil, as it is outside MOCK_MODE

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, predictionNow)
	require.NoError(t, err)
	assert.True(t, el.Blocked)
	assert.Equal(t, DeclineNoIncomeHistory, el.BlockedReason)
}

// The exact state a freshly linked account is in through the real
// InitiateLink/CompleteLink HTTP flow: linked, but never seeded with any
// transaction at all (no map entry, not even an empty slice). This must
// decline cleanly, the same as any other insufficient-history case — not
// 500, which is what it did before banklink.Mock.GetTransactions stopped
// treating an unseeded account as an error (see mock_test.go).
func TestGetEligibility_D2CWorker_FreshlyLinkedAccountWithNoHistoryBlocksCleanly(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, _ := seedD2CLinkedWorker(t)
	svc := &EWAService{D2CProvider: banklink.NewMock()} // nothing seeded for any account

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, predictionNow)
	require.NoError(t, err)
	assert.True(t, el.Blocked)
	assert.Equal(t, DeclineNoIncomeHistory, el.BlockedReason)
}

// The same scenario as the test above, but through RequestAdvance — the
// path that actually writes an ewa_advances row. eligibilityTx blocking
// with DeclineNoIncomeHistory returns before ScoreDependency runs, so
// without eligibilityTx defaulting Dependency.Tier to Healthy up front,
// this 500ed on ewa_advances_dependency_tier_check instead of recording a
// clean decline — reproduced live against a running server before being
// fixed here.
func TestRequestAdvance_D2CWorker_NoIncomeHistoryDeclinesCleanly(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, _ := seedD2CLinkedWorker(t)
	svc := &EWAService{D2CProvider: banklink.NewMock()}

	advance, _, err := svc.RequestAdvance(
		context.Background(), orgID, employeeID, money.FromNaira(5_000), "d2c-idem-no-history", "127.0.0.1")

	require.ErrorIs(t, err, ErrAdvanceDeclined)
	require.NotNil(t, advance)
	assert.Equal(t, models.AdvanceDeclined, advance.Status)
	assert.Equal(t, DeclineNoIncomeHistory, advance.DeclineReason)
	assert.Equal(t, models.TierHealthy, advance.DependencyTier)
}

func TestGetEligibility_D2CWorker_InsufficientHistoryBlocks(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, accountRef := seedD2CLinkedWorker(t)

	mock := banklink.NewMock()
	mock.Transactions[accountRef] = recurringCredits(1, 30, 1, money.FromNaira(300_000))
	svc := &EWAService{D2CProvider: mock}

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, predictionNow)
	require.NoError(t, err)
	assert.True(t, el.Blocked)
	assert.Equal(t, DeclineNoIncomeHistory, el.BlockedReason)
}

// End-to-end: a D2C worker's predicted income actually gates a real advance
// and posts the ledger, the same way a payroll-shaped org's accrual does.
// GetEligibility/RequestAdvance use time.Now() as asOf (not a fixture), so
// transactions are seeded relative to now rather than predictionNow.
func TestRequestAdvance_D2CWorker_ApprovedPostsLedger(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID, accountRef := seedD2CLinkedWorker(t)

	now := time.Now()
	mock := banklink.NewMock()
	mock.Transactions[accountRef] = []banklink.Transaction{
		{Date: now.AddDate(0, 0, -90), Amount: money.FromNaira(300_000), Direction: banklink.Credit},
		{Date: now.AddDate(0, 0, -60), Amount: money.FromNaira(300_000), Direction: banklink.Credit},
		{Date: now.AddDate(0, 0, -30), Amount: money.FromNaira(300_000), Direction: banklink.Credit},
	}
	svc := &EWAService{D2CProvider: mock}

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, now)
	require.NoError(t, err)
	require.False(t, el.Blocked, "blocked_reason=%s", el.BlockedReason)
	require.True(t, el.Available.IsPositive(), "expected some availability at a fully-accrued predicted payday")

	advance, _, err := svc.RequestAdvance(context.Background(), orgID, employeeID, el.MinimumDraw, "d2c-idem-1", "127.0.0.1")
	require.NoError(t, err)
	require.Equal(t, models.AdvanceApproved, advance.Status)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		recv, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
		require.NoError(t, err)
		bal, err := models.AccountBalance(tx, recv.ID)
		require.NoError(t, err)
		assert.Equal(t, money.NGNFromKobo(el.MinimumDraw), bal, "the advance must appear as a receivable")
		return nil
	}))
}
