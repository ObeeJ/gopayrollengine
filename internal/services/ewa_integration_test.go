//go:build integration

package services

import (
	"context"
	"os"
	"testing"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		os.Exit(m.Run())
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	models.DB = db
	models.InitEncryption()
	os.Exit(m.Run())
}

func skipIfNoDB(t *testing.T) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set — skipping EWA integration test")
	}
}

// seedWorker creates an org with one active salaried employee.
// ngnMoney builds a whole-Naira Money for test readability.
func ngnMoney(naira int64) money.Money {
	return money.Money{Minor: naira * 100, Currency: money.NGN}
}

func seedWorker(t *testing.T, salary money.Kobo) (orgID, employeeID string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	employeeID = "EMP-" + uuid.New().String()[:8]

	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		orgID, "ewa test org",
	).Error)

	require.NoError(t, models.DB.Create(&models.Employee{
		ID:             employeeID,
		OrganizationID: orgID,
		Name:           "Ada",
		Email:          models.EncryptedString("ada-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		Salary:         salary,
		IsActive:       true,
	}).Error)

	return orgID, employeeID
}

// --- Ledger -----------------------------------------------------------------

func TestPostTransaction_BalancedPostingAndDerivedBalance(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))

	err := models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		receivable, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
		require.NoError(t, err)
		cash, err := models.EnsureAccount(tx, orgID, "", models.AccountCashSettlement, money.NGN)
		require.NoError(t, err)

		_, err = models.PostTransaction(tx, models.PostingRequest{
			OrgID:          orgID,
			Kind:           "ewa_advance",
			IdempotencyKey: "adv-1",
			Entries: []models.EntryInput{
				{AccountID: receivable.ID, Direction: models.Debit, Amount: ngnMoney(20_000)},
				{AccountID: cash.ID, Direction: models.Credit, Amount: ngnMoney(20_000)},
			},
		})
		require.NoError(t, err)

		// Balances are derived from entries, never read off a stored column.
		recvBal, err := models.AccountBalance(tx, receivable.ID)
		require.NoError(t, err)
		assert.Equal(t, ngnMoney(20_000), recvBal,
			"debit-normal receivable should be positive after a debit")

		cashBal, err := models.AccountBalance(tx, cash.ID)
		require.NoError(t, err)
		assert.Equal(t, ngnMoney(20_000), cashBal,
			"credit-normal cash account should be positive after a credit")
		return nil
	})
	require.NoError(t, err)
}

// A retried disbursement must not post twice. This is the difference between a
// network blip and paying someone the same advance again.
func TestPostTransaction_IsIdempotent(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))

	post := func() string {
		var txID string
		require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
			recv, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
			require.NoError(t, err)
			cash, err := models.EnsureAccount(tx, orgID, "", models.AccountCashSettlement, money.NGN)
			require.NoError(t, err)

			ltx, err := models.PostTransaction(tx, models.PostingRequest{
				OrgID:          orgID,
				Kind:           "ewa_advance",
				IdempotencyKey: "same-key",
				Entries: []models.EntryInput{
					{AccountID: recv.ID, Direction: models.Debit, Amount: ngnMoney(10_000)},
					{AccountID: cash.ID, Direction: models.Credit, Amount: ngnMoney(10_000)},
				},
			})
			require.NoError(t, err)
			txID = ltx.ID
			return nil
		}))
		return txID
	}

	first := post()
	second := post()
	assert.Equal(t, first, second, "a replay must return the original transaction")

	var entryCount int64
	require.NoError(t, models.DB.Model(&models.LedgerEntry{}).
		Where("organization_id = ?", orgID).Count(&entryCount).Error)
	assert.Equal(t, int64(2), entryCount, "a replay must not write more entries")
}

// The deferred constraint trigger is the real guarantee. Even if application
// validation were bypassed, an unbalanced transaction must not commit.
func TestPostTransaction_UnbalancedIsRejectedByDatabase(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))

	err := models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		recv, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
		require.NoError(t, err)
		cash, err := models.EnsureAccount(tx, orgID, "", models.AccountCashSettlement, money.NGN)
		require.NoError(t, err)

		ltx := models.LedgerTransaction{
			OrganizationID: orgID, Kind: "forced", IdempotencyKey: "forced-1",
		}
		if err := tx.Create(&ltx).Error; err != nil {
			return err
		}
		// Insert the entries directly, bypassing ValidatePosting entirely.
		return tx.Create(&[]models.LedgerEntry{
			{TransactionID: ltx.ID, AccountID: recv.ID, OrganizationID: orgID,
				Direction: models.Debit, AmountMinor: 20_000_00, Currency: money.NGN},
			{TransactionID: ltx.ID, AccountID: cash.ID, OrganizationID: orgID,
				Direction: models.Credit, AmountMinor: 19_000_00, Currency: money.NGN},
		}).Error
	})

	require.Error(t, err, "the database must reject an unbalanced transaction at commit")
	assert.Contains(t, err.Error(), "unbalanced")

	var leaked int64
	require.NoError(t, models.DB.Model(&models.LedgerEntry{}).
		Where("organization_id = ?", orgID).Count(&leaked).Error)
	assert.Zero(t, leaked, "no entries may survive a rejected transaction")
}

// --- EWA end to end ---------------------------------------------------------

func TestRequestAdvance_ApprovedPostsLedgerAndCapsAtEarnings(t *testing.T) {
	skipIfNoDB(t)
	salary := money.FromNaira(300_000)
	orgID, employeeID := seedWorker(t, salary)
	svc := NewEWAService()

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, time.Now())
	require.NoError(t, err)
	require.False(t, el.Blocked, "a fresh worker mid-period should be eligible: %s", el.BlockedReason)

	// The defining invariant: never more than what has actually been earned.
	assert.LessOrEqual(t, int64(el.Available), int64(el.AccruedToDate),
		"available must never exceed accrued wages")
	require.True(t, el.Available.IsPositive(), "expected some availability mid-period")

	advance, _, err := svc.RequestAdvance(
		context.Background(), orgID, employeeID, el.MinimumDraw, "idem-1", "127.0.0.1")
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

// Asking for more than has been earned must be refused, and the refusal must be
// recorded — a pattern of declines is a signal, not noise to discard.
func TestRequestAdvance_OverEarningsIsDeclinedAndRecorded(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	advance, _, err := svc.RequestAdvance(
		context.Background(), orgID, employeeID, money.FromNaira(10_000_000), "idem-big", "127.0.0.1")

	require.ErrorIs(t, err, ErrAdvanceDeclined)
	require.NotNil(t, advance)
	assert.Equal(t, models.AdvanceDeclined, advance.Status)
	assert.Equal(t, DeclineExceedsEarned, advance.DeclineReason)

	// A declined advance is not money owed.
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		var count int64
		require.NoError(t, tx.Model(&models.LedgerEntry{}).
			Where("organization_id = ?", orgID).Count(&count).Error)
		assert.Zero(t, count, "a declined request must not post ledger entries")
		return nil
	}))
}

func TestRequestAdvance_IsIdempotent(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	first, _, err := svc.RequestAdvance(
		context.Background(), orgID, employeeID, money.FromNaira(1_000), "same-request", "127.0.0.1")
	require.NoError(t, err)

	second, _, err := svc.RequestAdvance(
		context.Background(), orgID, employeeID, money.FromNaira(1_000), "same-request", "127.0.0.1")
	require.NoError(t, err)

	assert.Equal(t, first.ID, second.ID, "a retried request must not create a second advance")

	var count int64
	require.NoError(t, models.DB.Model(&models.EWAAdvance{}).
		Where("organization_id = ?", orgID).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

// markDisbursed drives an approved advance to disbursed. Stands in for the
// Monnify transfer that Phase 2 will perform.
func markDisbursed(t *testing.T, orgID, advanceID string) {
	t.Helper()
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		adv := models.EWAAdvance{ID: advanceID}
		return models.TransitionAdvance(tx, &adv, models.AdvanceApproved, models.AdvanceDisbursed)
	}))
}

// An advance that was approved but never actually paid out must NOT be deducted
// from wages. Withholding pay for money the worker never received is wage theft,
// not an accounting rounding decision. It is cancelled and its receivable
// reversed instead.
func TestSettleAdvancesForPayrollItem_UndisbursedAdvanceIsCancelledNotDeducted(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	advance, _, err := svc.RequestAdvance(
		context.Background(), orgID, employeeID, money.FromNaira(4_000), "undisbursed-1", "127.0.0.1")
	require.NoError(t, err)
	require.Equal(t, models.AdvanceApproved, advance.Status, "left undisbursed on purpose")

	period := time.Now().Format(PeriodLayout)

	var withheld money.Kobo
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		var err error
		withheld, err = svc.SettleAdvancesForPayrollItem(tx, orgID, employeeID, period, "ITEM-X")
		return err
	}))

	assert.Equal(t, money.Zero, withheld,
		"nothing may be withheld for an advance that was never disbursed")

	var after models.EWAAdvance
	require.NoError(t, models.DB.First(&after, "id = ?", advance.ID).Error)
	assert.Equal(t, models.AdvanceCancelled, after.Status)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		recv, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
		require.NoError(t, err)
		bal, err := models.AccountBalance(tx, recv.ID)
		require.NoError(t, err)
		assert.Equal(t, ngnMoney(0), bal,
			"the cancellation must reverse the original receivable")
		return nil
	}))
}

// The loop that makes EWA solvent: an advance taken during the period must come
// back out of that period's payroll, and the receivable must return to zero.
func TestSettleAdvancesForPayrollItem_RecoversTheAdvance(t *testing.T) {
	skipIfNoDB(t)
	salary := money.FromNaira(300_000)
	orgID, employeeID := seedWorker(t, salary)
	svc := NewEWAService()

	drawn := money.FromNaira(5_000)
	advance, _, err := svc.RequestAdvance(
		context.Background(), orgID, employeeID, drawn, "settle-1", "127.0.0.1")
	require.NoError(t, err)
	require.Equal(t, models.AdvanceApproved, advance.Status)

	// Disbursement execution is Phase 2 (see docs/EWA_ROADMAP.md); until it is
	// wired, drive the transition directly so settlement can be exercised.
	markDisbursed(t, orgID, advance.ID)

	period := time.Now().Format(PeriodLayout)
	payrollItemID := "ITEM-" + uuid.New().String()[:8]

	var withheld money.Kobo
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		var err error
		withheld, err = svc.SettleAdvancesForPayrollItem(tx, orgID, employeeID, period, payrollItemID)
		return err
	}))

	assert.Equal(t, drawn, withheld, "the full advance must be withheld from payroll")

	var settled models.EWAAdvance
	require.NoError(t, models.DB.First(&settled, "id = ?", advance.ID).Error)
	assert.Equal(t, models.AdvanceSettled, settled.Status)
	require.NotNil(t, settled.SettledPayrollItemID)
	assert.Equal(t, payrollItemID, *settled.SettledPayrollItemID)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		recv, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
		require.NoError(t, err)
		bal, err := models.AccountBalance(tx, recv.ID)
		require.NoError(t, err)
		assert.Equal(t, ngnMoney(0), bal,
			"after settlement the worker owes nothing — debit and credit cancel")
		return nil
	}))
}

// Settlement must not run twice for the same advance: that would credit the
// receivable a second time and make a still-outstanding debt look repaid.
func TestSettleAdvancesForPayrollItem_IsIdempotent(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	drawn := money.FromNaira(3_000)
	advance, _, err := svc.RequestAdvance(
		context.Background(), orgID, employeeID, drawn, "settle-2", "127.0.0.1")
	require.NoError(t, err)
	markDisbursed(t, orgID, advance.ID)

	period := time.Now().Format(PeriodLayout)

	var first, second money.Kobo
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		var err error
		first, err = svc.SettleAdvancesForPayrollItem(tx, orgID, employeeID, period, "ITEM-A")
		return err
	}))
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		var err error
		second, err = svc.SettleAdvancesForPayrollItem(tx, orgID, employeeID, period, "ITEM-B")
		return err
	}))

	assert.Equal(t, drawn, first)
	assert.Equal(t, money.Zero, second, "a second settlement pass must withhold nothing")

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		recv, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
		require.NoError(t, err)
		bal, err := models.AccountBalance(tx, recv.ID)
		require.NoError(t, err)
		assert.Equal(t, ngnMoney(0), bal, "the receivable must not go negative")
		return nil
	}))
}

// setupRLSTestRole creates a NOSUPERUSER role the RLS assertions pivot into via
// SET LOCAL ROLE. Without it the test connects as postgres — a superuser, which
// bypasses RLS entirely, since FORCE applies to the table owner and not to
// superusers. A tenant-isolation test run as superuser passes for the wrong
// reason and proves nothing.
func setupRLSTestRole(t *testing.T) {
	t.Helper()
	require.NoError(t, models.DB.Exec(`
		DO $$
		BEGIN
			IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'rls_test_user') THEN
				CREATE ROLE rls_test_user NOSUPERUSER NOBYPASSRLS LOGIN;
			END IF;
		END $$;
		GRANT ALL ON ALL TABLES IN SCHEMA public TO rls_test_user;
		GRANT ALL ON ALL SEQUENCES IN SCHEMA public TO rls_test_user;
	`).Error)
}

// Tenant isolation must hold for the new ledger and EWA tables too: inside
// org B's scope, an unfiltered read must see none of org A's rows.
func TestEWAAdvances_AreTenantIsolated(t *testing.T) {
	skipIfNoDB(t)
	orgA, employeeA := seedWorker(t, money.FromNaira(300_000))
	orgB, _ := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	_, _, err := svc.RequestAdvance(
		context.Background(), orgA, employeeA, money.FromNaira(1_000), "iso-1", "127.0.0.1")
	require.NoError(t, err)

	setupRLSTestRole(t)

	require.NoError(t, models.WithOrgScope(context.Background(), orgB, func(tx *gorm.DB) error {
		// Pivot off the superuser connection for the duration of this tx.
		if err := tx.Exec("SET LOCAL ROLE rls_test_user").Error; err != nil {
			return err
		}

		var advances []models.EWAAdvance
		if err := tx.Find(&advances).Error; err != nil {
			return err
		}
		assert.Empty(t, advances, "org B must not see org A's advances")

		var entries []models.LedgerEntry
		if err := tx.Find(&entries).Error; err != nil {
			return err
		}
		assert.Empty(t, entries, "org B must not see org A's ledger entries")

		var accounts []models.LedgerAccount
		if err := tx.Find(&accounts).Error; err != nil {
			return err
		}
		assert.Empty(t, accounts, "org B must not see org A's ledger accounts")
		return nil
	}))
}

// The mirror assertion: within its own scope, org A must still see its own
// rows through the non-superuser role. A policy that hides everything from
// everyone is not isolation, it is an outage.
func TestEWAAdvances_OwnScopeStillReadable(t *testing.T) {
	skipIfNoDB(t)
	orgA, employeeA := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	_, _, err := svc.RequestAdvance(
		context.Background(), orgA, employeeA, money.FromNaira(1_000), "iso-2", "127.0.0.1")
	require.NoError(t, err)

	setupRLSTestRole(t)

	require.NoError(t, models.WithOrgScope(context.Background(), orgA, func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL ROLE rls_test_user").Error; err != nil {
			return err
		}

		var advances []models.EWAAdvance
		if err := tx.Find(&advances).Error; err != nil {
			return err
		}
		assert.Len(t, advances, 1, "org A must see exactly its own advance")

		var entries []models.LedgerEntry
		if err := tx.Find(&entries).Error; err != nil {
			return err
		}
		assert.Len(t, entries, 2, "org A must see both sides of its own posting")
		return nil
	}))
}

// setProtectedPayday records a worker-set floor.
func setProtectedPayday(t *testing.T, orgID, employeeID string, protect money.Kobo) {
	t.Helper()
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Save(&models.EWAWorkerPreference{
			OrganizationID:       orgID,
			EmployeeID:           employeeID,
			ProtectedPaydayMinor: int64(protect),
		}).Error
	}))
}

// A worker's own floor must cap access even when policy and tier would allow
// more. This is the one guardrail whose legitimacy does not depend on the
// dependency model being correct, so it must be the one that binds hardest.
func TestEligibility_WorkerFloorCapsAvailable(t *testing.T) {
	skipIfNoDB(t)
	salary := money.FromNaira(300_000)
	orgID, employeeID := seedWorker(t, salary)
	svc := NewEWAService()

	before, err := svc.GetEligibility(context.Background(), orgID, employeeID, time.Now())
	require.NoError(t, err)
	require.True(t, before.Available.IsPositive(), "expected headroom before the floor is set")

	// Ask to protect nearly the whole salary, leaving only N10,000 spendable.
	setProtectedPayday(t, orgID, employeeID, money.FromNaira(290_000))

	after, err := svc.GetEligibility(context.Background(), orgID, employeeID, time.Now())
	require.NoError(t, err)

	assert.Equal(t, money.FromNaira(290_000), after.ProtectedPayday)
	assert.LessOrEqual(t, int64(after.Available), int64(money.FromNaira(10_000)),
		"the worker's floor must bind even though the policy cap is higher")
	assert.Less(t, int64(after.Available), int64(before.Available),
		"setting a floor must reduce available, not merely be recorded")
}

// The UK evidence is that users grasp "money available now" but are surprised by
// the smaller paycheck. Both numbers must be present and internally consistent.
func TestEligibility_ProjectsThePaycheckConsequence(t *testing.T) {
	skipIfNoDB(t)
	salary := money.FromNaira(300_000)
	orgID, employeeID := seedWorker(t, salary)
	svc := NewEWAService()

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, time.Now())
	require.NoError(t, err)

	// Nothing drawn yet, so the projected payday is the whole salary.
	assert.Equal(t, salary, el.ProjectedPayday)

	// Drawing the full allowance must reduce payday by exactly that allowance.
	expected, err := el.ProjectedPayday.Sub(el.Available)
	require.NoError(t, err)
	assert.Equal(t, expected, el.ProjectedPaydayIfMaxDrawn,
		"worst-case payday must equal payday minus everything still drawable")
	assert.Less(t, int64(el.ProjectedPaydayIfMaxDrawn), int64(el.ProjectedPayday))
}

// After a draw, the projected payday must fall by the drawn amount — the number
// the worker is most likely to be surprised by has to stay truthful.
func TestEligibility_ProjectedPaydayFallsAfterDrawing(t *testing.T) {
	skipIfNoDB(t)
	salary := money.FromNaira(300_000)
	orgID, employeeID := seedWorker(t, salary)
	svc := NewEWAService()

	drawn := money.FromNaira(5_000)
	_, _, err := svc.RequestAdvance(
		context.Background(), orgID, employeeID, drawn, "proj-1", "127.0.0.1")
	require.NoError(t, err)

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, time.Now())
	require.NoError(t, err)

	expected, err := salary.Sub(drawn)
	require.NoError(t, err)
	assert.Equal(t, expected, el.ProjectedPayday,
		"projected payday must reflect what has already been taken")
}
