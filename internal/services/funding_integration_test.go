//go:build integration

package services

import (
	"context"
	"testing"

	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// enableFundingCoverage writes an explicit ewa_policies row with
// RequireFundingCoverage set — the in-code DefaultEWAPolicy fallback always
// leaves it false (see migration 000019), so exercising the guardrail
// requires a real row, not just an override on the zero-value struct.
func enableFundingCoverage(t *testing.T, orgID string) {
	t.Helper()
	policy := models.DefaultEWAPolicy(orgID)
	policy.RequireFundingCoverage = true
	require.NoError(t, models.DB.Create(&policy).Error)
}

// addEmployee adds a second salaried employee to an existing org — used where
// a test needs two draws against the same org-level funding pool without one
// draw's own cooling-off window (scoped per employee) getting in the way.
func addEmployee(t *testing.T, orgID string, salary money.Kobo) (employeeID string) {
	t.Helper()
	employeeID = "EMP-" + uuid.New().String()[:8]
	require.NoError(t, models.DB.Create(&models.Employee{
		ID:             employeeID,
		OrganizationID: orgID,
		Name:           "Bola",
		Email:          models.EncryptedString("bola-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		Salary:         salary,
		IsActive:       true,
	}).Error)
	return employeeID
}

// --- models.RecordEmployerFunding / FundingExposure --------------------------

func TestRecordEmployerFunding_CreditsPoolAndOffsetsExposure(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedWorker(t, money.FromNaira(300_000))

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.RecordEmployerFunding(tx, orgID, ngnMoney(50_000), "funding-ref-1")
	}))

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		exposure, err := models.FundingExposure(tx, orgID, money.NGN)
		require.NoError(t, err)
		// No advances yet, so a deposit alone must push exposure negative —
		// the org has funded more than it has drawn.
		assert.Equal(t, money.Money{Minor: -50_000 * 100, Currency: money.NGN}, exposure)
		return nil
	}))
}

func TestRecordEmployerFunding_IdempotentOnProviderReference(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedWorker(t, money.FromNaira(300_000))
	ref := "funding-ref-" + uuid.New().String()[:8]

	post := func() error {
		return models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
			return models.RecordEmployerFunding(tx, orgID, ngnMoney(10_000), ref)
		})
	}
	require.NoError(t, post())
	require.NoError(t, post(), "a duplicate delivery for the same provider reference must not error")

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		exposure, err := models.FundingExposure(tx, orgID, money.NGN)
		require.NoError(t, err)
		assert.Equal(t, money.Money{Minor: -10_000 * 100, Currency: money.NGN}, exposure,
			"the second delivery must not post a second time")
		return nil
	}))
}

// An advance disbursed against an org's pool increases exposure exactly the
// way a deposit decreases it — postAdvanceLedger's existing entries are
// untouched by this feature (see migration 000019), so this is really a test
// that the two paths compose correctly, not a test of either path alone.
func TestFundingExposure_NetsAdvancesAgainstDeposits(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	// A small draw, well within what even one elapsed working day accrues on
	// a ₦300,000 salary — see the comment on el.MinimumDraw usage elsewhere in
	// this suite; a date-sensitive accrual amount here would make the test
	// flaky depending on which day of the month it runs.
	_, _, err := svc.RequestAdvance(context.Background(), orgID, employeeID, money.FromNaira(3_000), "adv-1", "127.0.0.1")
	require.NoError(t, err)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.RecordEmployerFunding(tx, orgID, ngnMoney(2_000), "funding-ref-net")
	}))

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		exposure, err := models.FundingExposure(tx, orgID, money.NGN)
		require.NoError(t, err)
		// ₦3,000 advanced minus ₦2,000 deposited = ₦1,000 still unfunded.
		assert.Equal(t, money.Money{Minor: 1_000 * 100, Currency: money.NGN}, exposure)
		return nil
	}))
}

// --- RequestAdvance funding guardrail -----------------------------------------

// The default policy never enables this check — an org that has never heard
// of the funding pool feature must not be blocked by it.
func TestRequestAdvance_FundingCoverageOffByDefault(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	advance, _, err := svc.RequestAdvance(context.Background(), orgID, employeeID, money.FromNaira(1_000), "off-1", "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, models.AdvanceApproved, advance.Status)
}

func TestRequestAdvance_BlockedWhenPoolUnfunded(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	enableFundingCoverage(t, orgID)
	svc := NewEWAService()

	advance, el, err := svc.RequestAdvance(context.Background(), orgID, employeeID, money.FromNaira(10_000), "unfunded-1", "127.0.0.1")
	require.ErrorIs(t, err, ErrAdvanceDeclined)
	assert.Equal(t, DeclineFundingPoolExhausted, advance.DeclineReason)
	assert.True(t, el.Blocked)
}

func TestRequestAdvance_AllowedUpToFundedAmount(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	enableFundingCoverage(t, orgID)
	svc := NewEWAService()

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.RecordEmployerFunding(tx, orgID, ngnMoney(3_000), "funding-ref-cover")
	}))

	// Within the funded amount: succeeds.
	advance, _, err := svc.RequestAdvance(context.Background(), orgID, employeeID, money.FromNaira(2_000), "funded-1", "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, models.AdvanceApproved, advance.Status)

	// A second employee in the same org draws against the remaining ₦1,000 of
	// pool headroom — a different employee so this exercises the shared,
	// org-level funding pool rather than tripping the first employee's own
	// per-employee cooling-off window.
	otherEmployeeID := addEmployee(t, orgID, money.FromNaira(300_000))
	advance2, el, err := svc.RequestAdvance(context.Background(), orgID, otherEmployeeID, money.FromNaira(2_000), "funded-2", "127.0.0.1")
	require.ErrorIs(t, err, ErrAdvanceDeclined)
	assert.Equal(t, DeclineFundingPoolExhausted, advance2.DeclineReason)
	// Blocked means no draw at all is possible; here ₦1,000 of pool headroom
	// remains, so el.Blocked is correctly false — only this specific ₦2,000
	// request exceeds it. FundingBound is the flag that carries the real
	// cause through to the decline reason in that case; see its doc comment.
	assert.False(t, el.Blocked)
	assert.True(t, el.FundingBound)
}

// --- FundingService.ProvisionAccount ------------------------------------------

func TestProvisionAccount_IsIdempotent(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedWorker(t, money.FromNaira(300_000))
	svc := NewFundingService(monnify.NewClient()) // MOCK_MODE per TestMain's env

	first, err := svc.ProvisionAccount(context.Background(), orgID, "finance@example.com")
	require.NoError(t, err)
	require.NotEmpty(t, first.AccountNumber)

	second, err := svc.ProvisionAccount(context.Background(), orgID, "finance@example.com")
	require.NoError(t, err)
	assert.Equal(t, first.AccountNumber, second.AccountNumber, "a second call must return the same account, not mint another")
}

func TestGetFundingStatus_ReflectsExposure(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedWorker(t, money.FromNaira(300_000))
	fundingSvc := NewFundingService(monnify.NewClient())

	status, err := fundingSvc.GetFundingStatus(context.Background(), orgID)
	require.NoError(t, err)
	assert.Nil(t, status.Account, "no account provisioned yet")
	assert.Equal(t, money.Money{Minor: 0, Currency: money.NGN}, status.Exposure)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.RecordEmployerFunding(tx, orgID, ngnMoney(5_000), "funding-ref-status")
	}))

	status, err = fundingSvc.GetFundingStatus(context.Background(), orgID)
	require.NoError(t, err)
	assert.Equal(t, money.Money{Minor: -5_000 * 100, Currency: money.NGN}, status.Exposure)
}
