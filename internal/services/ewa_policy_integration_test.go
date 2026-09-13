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

func intPtr(n int) *int                { return &n }
func koboPtr(k money.Kobo) *money.Kobo { return &k }
func boolPtr(b bool) *bool             { return &b }
func strPtr(s string) *string          { return &s }

func TestGetPolicy_ReturnsDefaultWhenNoneSaved(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	policy, err := svc.GetPolicy(context.Background(), orgID)
	require.NoError(t, err)
	assert.Equal(t, models.DefaultEWAPolicy(orgID).MaxAccrualPct, policy.MaxAccrualPct)
	assert.False(t, policy.RequireFundingCoverage)
}

func TestUpdatePolicy_FirstEverConfigurationPersists(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	updated, err := svc.UpdatePolicy(context.Background(), orgID, PolicyUpdate{
		MaxAccrualPct: intPtr(40),
	}, "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, 40, updated.MaxAccrualPct)

	reloaded, err := svc.GetPolicy(context.Background(), orgID)
	require.NoError(t, err)
	assert.Equal(t, 40, reloaded.MaxAccrualPct)
}

// A second update must change only what it touches — the whole point of
// PolicyUpdate's pointer fields being optional.
func TestUpdatePolicy_PartialUpdateLeavesOtherFieldsUntouched(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	_, err := svc.UpdatePolicy(context.Background(), orgID, PolicyUpdate{
		MaxAccrualPct:          intPtr(40),
		RequireFundingCoverage: boolPtr(true),
	}, "127.0.0.1")
	require.NoError(t, err)

	updated, err := svc.UpdatePolicy(context.Background(), orgID, PolicyUpdate{
		MinDrawKobo: koboPtr(money.FromNaira(2_000)),
	}, "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, 40, updated.MaxAccrualPct, "an earlier field must survive an unrelated update")
	assert.True(t, updated.RequireFundingCoverage, "an earlier field must survive an unrelated update")
	assert.Equal(t, money.FromNaira(2_000), updated.MinDrawKobo)
}

func TestUpdatePolicy_RejectsOutOfRangeValues(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	cases := []PolicyUpdate{
		{MaxAccrualPct: intPtr(0)},
		{MaxAccrualPct: intPtr(101)},
		{AbsoluteCapKobo: koboPtr(0)},
		{MinDrawKobo: koboPtr(-1)},
		{MaxDrawsPerPeriod: intPtr(0)},
		{CoolingOffHours: intPtr(-1)},
		{EmergencyFloorKobo: koboPtr(-1)},
	}
	for _, upd := range cases {
		_, err := svc.UpdatePolicy(context.Background(), orgID, upd, "127.0.0.1")
		require.ErrorIsf(t, err, ErrInvalidPolicyValue, "update %+v should have been rejected", upd)
	}

	// None of the rejected updates should have partially applied.
	policy, err := svc.GetPolicy(context.Background(), orgID)
	require.NoError(t, err)
	assert.Equal(t, models.DefaultEWAPolicy(orgID).MaxAccrualPct, policy.MaxAccrualPct)
}

func TestUpdatePolicy_AuditsBeforeAndAfter(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	_, err := svc.UpdatePolicy(context.Background(), orgID, PolicyUpdate{
		MaxAccrualPct: intPtr(40),
	}, "127.0.0.1")
	require.NoError(t, err)

	var event models.AuditEvent
	require.NoError(t, models.DB.Where("organization_id = ? AND entity_type = ? AND action = ?",
		orgID, "EWAPolicy", "policy_updated").First(&event).Error)
	assert.Contains(t, event.After, `"max_accrual_pct":40`)
}

func TestUpdatePolicy_SetsCounsellingResource(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	updated, err := svc.UpdatePolicy(context.Background(), orgID, PolicyUpdate{
		CounsellingResourceName: strPtr("Acme EAP"),
		CounsellingContact:      strPtr("0800-000-0000"),
	}, "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, "Acme EAP", updated.CounsellingResourceName)
	assert.Equal(t, "0800-000-0000", updated.CounsellingContact)

	reloaded, err := svc.GetPolicy(context.Background(), orgID)
	require.NoError(t, err)
	assert.Equal(t, "Acme EAP", reloaded.CounsellingResourceName)
}

// End to end: a worker's eligibility response must carry the employer's
// configured counselling resource once their dependency tier reaches
// Strained — the whole point of wiring TierCounsellingReferral through
// eligibilityTx rather than leaving it a dependency-package-only concern.
func TestGetEligibility_SurfacesCounsellingReferralAtStrainedTier(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	_, err := svc.UpdatePolicy(context.Background(), orgID, PolicyUpdate{
		CounsellingResourceName: strPtr("Acme EAP"),
		CounsellingContact:      strPtr("0800-000-0000"),
	}, "127.0.0.1")
	require.NoError(t, err)

	// Seed a saturated draw history directly — mirrors the dependency
	// package's own fixture for reaching Dependent (well past Strained).
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

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, time.Now())
	require.NoError(t, err)
	require.Equal(t, models.TierDependent, el.Dependency.Tier, "test setup must actually reach a referral-eligible tier")
	require.NotNil(t, el.Dependency.CounsellingReferral)
	assert.Equal(t, "Acme EAP", el.Dependency.CounsellingReferral.ResourceName)
}
