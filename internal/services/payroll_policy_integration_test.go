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

func TestGetPayrollPolicy_ReturnsDefaultWhenNoneSaved(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedHourlyWorker(t, money.FromNaira(1_000))
	svc := NewPayrollService(nil, nil)

	policy, err := svc.GetPayrollPolicy(context.Background(), orgID)
	require.NoError(t, err)
	assert.Equal(t, models.DefaultPayrollPolicy(orgID).OvertimeThresholdMinutesPerWeek, policy.OvertimeThresholdMinutesPerWeek)
	assert.Equal(t, 15000, policy.OvertimeMultiplierBps)
	assert.Equal(t, 10000, policy.NightShiftMultiplierBps)
}

func TestUpdatePayrollPolicy_FirstEverConfigurationPersists(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedHourlyWorker(t, money.FromNaira(1_000))
	svc := NewPayrollService(nil, nil)

	updated, err := svc.UpdatePayrollPolicy(context.Background(), orgID, PayrollPolicyUpdate{
		NightShiftMultiplierBps: intPtr(12500),
	}, "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, 12500, updated.NightShiftMultiplierBps)

	reloaded, err := svc.GetPayrollPolicy(context.Background(), orgID)
	require.NoError(t, err)
	assert.Equal(t, 12500, reloaded.NightShiftMultiplierBps)
}

// A second update must change only what it touches, same as EWAPolicy.
func TestUpdatePayrollPolicy_PartialUpdateLeavesOtherFieldsUnchanged(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedHourlyWorker(t, money.FromNaira(1_000))
	svc := NewPayrollService(nil, nil)

	_, err := svc.UpdatePayrollPolicy(context.Background(), orgID, PayrollPolicyUpdate{
		NightShiftMultiplierBps: intPtr(12500),
	}, "127.0.0.1")
	require.NoError(t, err)

	updated, err := svc.UpdatePayrollPolicy(context.Background(), orgID, PayrollPolicyUpdate{
		WeekendShiftMultiplierBps: intPtr(13000),
	}, "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, 12500, updated.NightShiftMultiplierBps, "untouched by this update")
	assert.Equal(t, 13000, updated.WeekendShiftMultiplierBps)
}

// The DB CHECK constraints floor every multiplier at 10000 (1.0x) so this
// table can never silently cut pay; the service must reject a lower value
// before it ever reaches that constraint.
func TestUpdatePayrollPolicy_RejectsMultiplierBelowOnex(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedHourlyWorker(t, money.FromNaira(1_000))
	svc := NewPayrollService(nil, nil)

	for _, upd := range []PayrollPolicyUpdate{
		{OvertimeMultiplierBps: intPtr(9999)},
		{NightShiftMultiplierBps: intPtr(5000)},
		{WeekendShiftMultiplierBps: intPtr(0)},
		{HolidayShiftMultiplierBps: intPtr(-100)},
	} {
		_, err := svc.UpdatePayrollPolicy(context.Background(), orgID, upd, "127.0.0.1")
		require.ErrorIs(t, err, ErrInvalidPolicyValue)
	}
}

func TestUpdatePayrollPolicy_RejectsNonPositiveThreshold(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedHourlyWorker(t, money.FromNaira(1_000))
	svc := NewPayrollService(nil, nil)

	_, err := svc.UpdatePayrollPolicy(context.Background(), orgID, PayrollPolicyUpdate{
		OvertimeThresholdMinutesPerWeek: intPtr(0),
	}, "127.0.0.1")
	require.ErrorIs(t, err, ErrInvalidPolicyValue)
}
