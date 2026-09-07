//go:build integration

package services

import (
	"context"
	"testing"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func intPtr(n int) *int                { return &n }
func koboPtr(k money.Kobo) *money.Kobo { return &k }
func boolPtr(b bool) *bool             { return &b }

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
