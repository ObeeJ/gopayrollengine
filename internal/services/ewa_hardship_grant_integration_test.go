//go:build integration

package services

import (
	"context"
	"testing"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestIssueHardshipGrant_ValidatesAndPersists(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	grant, err := svc.IssueHardshipGrant(
		context.Background(), orgID, employeeID, money.FromNaira(25_000), "rent shortfall", "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, money.FromNaira(25_000), grant.AmountKobo)
	assert.Equal(t, "rent shortfall", grant.Reason)

	grants, err := svc.ListHardshipGrants(context.Background(), orgID, employeeID)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	assert.Equal(t, grant.ID, grants[0].ID)
}

func TestIssueHardshipGrant_RejectsInvalidInput(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	_, err := svc.IssueHardshipGrant(context.Background(), orgID, employeeID, 0, "reason", "127.0.0.1")
	require.ErrorIs(t, err, ErrInvalidHardshipGrant, "non-positive amount")

	_, err = svc.IssueHardshipGrant(context.Background(), orgID, employeeID, money.FromNaira(1_000), "", "127.0.0.1")
	require.ErrorIs(t, err, ErrInvalidHardshipGrant, "empty reason")
}

// The defining accounting property: a grant is never a receivable. The
// worker's advance_receivable balance must be completely untouched by it —
// unlike an EWA advance, there is nothing here for a future payroll run to
// recover.
func TestIssueHardshipGrant_NeverCreatesAReceivable(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	_, err := svc.IssueHardshipGrant(
		context.Background(), orgID, employeeID, money.FromNaira(25_000), "hardship", "127.0.0.1")
	require.NoError(t, err)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		recv, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
		require.NoError(t, err)
		bal, err := models.AccountBalance(tx, recv.ID)
		require.NoError(t, err)
		assert.Equal(t, money.Money{Currency: money.NGN}, bal, "a grant must never touch advance_receivable")

		expense, err := models.EnsureAccount(tx, orgID, "", models.AccountHardshipGrantExpense, money.NGN)
		require.NoError(t, err)
		expBal, err := models.AccountBalance(tx, expense.ID)
		require.NoError(t, err)
		assert.Equal(t, money.Money{Minor: 25_000 * 100, Currency: money.NGN}, expBal,
			"the grant is booked as an immediate expense")
		return nil
	}))
}

func TestIssueHardshipGrant_RespectsFundingCoverageWhenEnabled(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	_, err := svc.UpdatePolicy(context.Background(), orgID, PolicyUpdate{
		RequireFundingCoverage: boolPtr(true),
	}, "127.0.0.1")
	require.NoError(t, err)

	// No deposits on record yet — any grant should be blocked.
	_, err = svc.IssueHardshipGrant(
		context.Background(), orgID, employeeID, money.FromNaira(10_000), "hardship", "127.0.0.1")
	require.ErrorIs(t, err, ErrHardshipGrantPoolExhausted)

	// Fund the pool, then the same grant should succeed.
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.RecordEmployerFunding(tx, orgID, money.Money{Minor: 10_000 * 100, Currency: money.NGN}, "dep-1")
	}))
	grant, err := svc.IssueHardshipGrant(
		context.Background(), orgID, employeeID, money.FromNaira(10_000), "hardship", "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, money.FromNaira(10_000), grant.AmountKobo)
}

// GetEligibility must suggest a hardship grant when the worker has hit this
// period's draw limit while already Strained — the whole point of the
// "genuine alternative to a fourth advance" framing.
func TestGetEligibility_SuggestsHardshipGrantAtDrawLimitWhileStrained(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	_, err := svc.UpdatePolicy(context.Background(), orgID, PolicyUpdate{
		MaxDrawsPerPeriod: intPtr(1),
	}, "127.0.0.1")
	require.NoError(t, err)

	now := time.Now()
	period := now.Format(PeriodLayout)
	// 60 daily draws — the same saturated-history fixture other tests use to
	// reliably reach Dependent — also trivially exceeds MaxDrawsPerPeriod=1,
	// so this one seeding covers both the draw-limit block and the tier.
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		for i := 0; i < 60; i++ {
			adv := models.EWAAdvance{
				OrganizationID: orgID,
				EmployeeID:     employeeID,
				Period:         period,
				AmountKobo:     money.FromNaira(50_000),
				Status:         models.AdvanceSettled,
				DependencyTier: models.TierHealthy,
				RequestedAt:    now.AddDate(0, 0, -i),
			}
			if err := tx.Create(&adv).Error; err != nil {
				return err
			}
		}
		return nil
	}))

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, now)
	require.NoError(t, err)
	require.True(t, el.Blocked)
	require.Equal(t, DeclineDrawLimit, el.BlockedReason)
	require.Contains(t, []models.DependencyTier{models.TierStrained, models.TierDependent}, el.Dependency.Tier,
		"test setup must actually reach a referral-eligible tier")
	assert.True(t, el.HardshipGrantSuggested)
}
