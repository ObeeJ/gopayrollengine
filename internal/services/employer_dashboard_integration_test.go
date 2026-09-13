//go:build integration

package services

import (
	"context"
	"testing"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedOrgWithEmployees creates one org with n active salaried employees, all
// earning `salary`. Returns the org id and each employee's id.
func seedOrgWithEmployees(t *testing.T, n int, salary money.Kobo) (orgID string, employeeIDs []string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		orgID, "dashboard test org",
	).Error)

	for i := 0; i < n; i++ {
		empID := "EMP-" + uuid.New().String()[:8]
		require.NoError(t, models.DB.Create(&models.Employee{
			ID:             empID,
			OrganizationID: orgID,
			Name:           "Worker",
			Email:          models.EncryptedString(uuid.New().String() + "@example.com"),
			AccountNumber:  models.EncryptedString("0123456789"),
			BankCode:       models.EncryptedString("058"),
			Salary:         salary,
			IsActive:       true,
		}).Error)
		employeeIDs = append(employeeIDs, empID)
	}
	return orgID, employeeIDs
}

// Below the k-anonymity threshold, the whole report is suppressed — no
// breakdown at all, however coarse, is safe to show for a team this small.
func TestGetWorkforceDependencyReport_SuppressedBelowKAnonymity(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedOrgWithEmployees(t, MinKAnonymity-1, money.FromNaira(300_000))

	svc := NewEmployerDashboardService(repository.NewEmployeeRepository(models.DB))
	report, err := svc.GetWorkforceDependencyReport(context.Background(), orgID, time.Now())
	require.NoError(t, err)

	assert.True(t, report.Suppressed)
	assert.NotEmpty(t, report.SuppressedReason)
	assert.Empty(t, report.Breakdown)
	assert.Equal(t, MinKAnonymity-1, report.ActiveEmployees)
}

// At exactly the threshold, with nobody ever having drawn, every employee is
// Healthy — a true zero in the other tiers must be reported, not suppressed.
func TestGetWorkforceDependencyReport_AllHealthyWhenNoDraws(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedOrgWithEmployees(t, MinKAnonymity, money.FromNaira(300_000))

	svc := NewEmployerDashboardService(repository.NewEmployeeRepository(models.DB))
	report, err := svc.GetWorkforceDependencyReport(context.Background(), orgID, time.Now())
	require.NoError(t, err)

	require.False(t, report.Suppressed)
	require.Len(t, report.Breakdown, 4)
	for _, bucket := range report.Breakdown {
		require.NotNil(t, bucket.Count, "a true zero must not be suppressed")
		require.NotNil(t, bucket.Percent)
		assert.False(t, bucket.Suppressed)
		if bucket.Tier == models.TierHealthy {
			assert.Equal(t, MinKAnonymity, *bucket.Count)
			assert.Equal(t, 100.0, *bucket.Percent)
		} else {
			assert.Equal(t, 0, *bucket.Count)
		}
	}
}

// seedSaturatedDraws drives one employee to the Dependent tier via 60 daily
// draws — mirrors TestScoreDependency_SaturatedUserReachesDependent's
// fixture, inserted directly since scoring only needs rows to read back, not
// a full RequestAdvance flow (which would hit cooling-off immediately).
func seedSaturatedDraws(t *testing.T, orgID, employeeID string) {
	t.Helper()
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		now := time.Now()
		for i := 0; i < 60; i++ {
			adv := models.EWAAdvance{
				OrganizationID: orgID,
				EmployeeID:     employeeID,
				Period:         now.Format(PeriodLayout),
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
}

// A single worker who has drawn heavily enough to reach the Dependent tier
// must NOT be individually identifiable in the breakdown. With exactly 10
// employees (9 Healthy, 1 Dependent), both buckets are individually below
// the threshold, so primary suppression alone hides both. Two buckets already
// hidden is enough ambiguity on its own (the observer only learns their
// combined total, 10, which every split from 1/9 to 9/1 satisfies), so no
// complementary suppression is needed — the two real zero buckets
// (Elevated, Strained) are safe to disclose exactly.
func TestGetWorkforceDependencyReport_SuppressesSmallBucket(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeIDs := seedOrgWithEmployees(t, MinKAnonymity, money.FromNaira(300_000))
	seedSaturatedDraws(t, orgID, employeeIDs[0])

	svc := NewEmployerDashboardService(repository.NewEmployeeRepository(models.DB))
	report, err := svc.GetWorkforceDependencyReport(context.Background(), orgID, time.Now())
	require.NoError(t, err)
	require.False(t, report.Suppressed)

	for _, bucket := range report.Breakdown {
		switch bucket.Tier {
		case models.TierHealthy, models.TierDependent:
			assert.True(t, bucket.Suppressed, "tier %s: individually under k=10", bucket.Tier)
			assert.Nil(t, bucket.Count)
			assert.Nil(t, bucket.Percent)
		case models.TierElevated, models.TierStrained:
			require.NotNil(t, bucket.Count, "a true zero carries no re-identification risk on its own")
			assert.Equal(t, 0, *bucket.Count)
		}
	}
}

// The subtler case: with a larger org (29 Healthy, 1 Dependent, 0 Elevated,
// 0 Strained), Healthy alone would clear k=10 easily on its own. But
// disclosing Healthy=29 exactly, alongside the two real zeros, would let
// anyone compute the suppressed Dependent count via total(30)-29-0-0=1.
// Complementary suppression closes that gap by additionally hiding
// whichever otherwise-disclosed bucket has the smallest count — here one of
// the two zero buckets — so the reader learns only that the two hidden
// buckets sum to 1, never which one holds it. Hiding the smaller-magnitude
// bucket (a zero) rather than Healthy=29 preserves the most useful
// disclosure while still closing the arithmetic gap.
func TestGetWorkforceDependencyReport_ComplementarySuppressionPreventsArithmeticLeak(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeIDs := seedOrgWithEmployees(t, MinKAnonymity+20, money.FromNaira(300_000))
	seedSaturatedDraws(t, orgID, employeeIDs[0])

	svc := NewEmployerDashboardService(repository.NewEmployeeRepository(models.DB))
	report, err := svc.GetWorkforceDependencyReport(context.Background(), orgID, time.Now())
	require.NoError(t, err)
	require.False(t, report.Suppressed)

	suppressedCount := 0
	for _, bucket := range report.Breakdown {
		if bucket.Tier == models.TierHealthy {
			require.NotNil(t, bucket.Count, "Healthy=29 clears k=10 on its own and is the most useful figure to keep")
			assert.Equal(t, MinKAnonymity+19, *bucket.Count)
			continue
		}
		if bucket.Tier == models.TierDependent {
			assert.True(t, bucket.Suppressed, "the single dependent worker must never be disclosed")
			suppressedCount++
			continue
		}
		// Elevated and Strained are both real zeros; exactly one of them must
		// be sacrificed as complementary cover so Dependent's exact value
		// can't be recovered by subtracting every other bucket from the total.
		if bucket.Suppressed {
			suppressedCount++
			assert.Nil(t, bucket.Count)
		} else {
			require.NotNil(t, bucket.Count)
			assert.Equal(t, 0, *bucket.Count)
		}
	}
	assert.Equal(t, 2, suppressedCount, "exactly two buckets must be hidden so Dependent's value stays ambiguous")
}
