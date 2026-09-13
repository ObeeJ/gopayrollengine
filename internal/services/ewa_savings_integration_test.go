//go:build integration

package services

import (
	"context"
	"testing"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestSetSavingsPreference_FixedPercentPersists(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	pref, err := svc.SetSavingsPreference(context.Background(), orgID, employeeID, SavingsPreferenceUpdate{
		Enabled: true, Mode: models.SavingsFixedPercent, Amount: 10,
	})
	require.NoError(t, err)
	assert.True(t, pref.Enabled)
	assert.Equal(t, models.SavingsFixedPercent, pref.Mode)
	assert.Equal(t, 10, pref.FixedPercent)

	reloaded, err := svc.GetSavingsPreference(context.Background(), orgID, employeeID)
	require.NoError(t, err)
	assert.True(t, reloaded.Enabled)
	assert.Equal(t, 10, reloaded.FixedPercent)
}

func TestSetSavingsPreference_DefaultsToDisabled(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	pref, err := svc.GetSavingsPreference(context.Background(), orgID, employeeID)
	require.NoError(t, err)
	assert.False(t, pref.Enabled, "a worker who never opted in must default to disabled")
}

func TestSetSavingsPreference_RejectsPercentAboveCap(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	_, err := svc.SetSavingsPreference(context.Background(), orgID, employeeID, SavingsPreferenceUpdate{
		Enabled: true, Mode: models.SavingsFixedPercent, Amount: 95,
	})
	require.ErrorIs(t, err, ErrInvalidSavingsPreference,
		"automated savings must never be able to zero out a paycheck, same as an EWA draw")
}

func TestSetSavingsPreference_RejectsZeroRoundUpUnit(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	_, err := svc.SetSavingsPreference(context.Background(), orgID, employeeID, SavingsPreferenceUpdate{
		Enabled: true, Mode: models.SavingsRoundUp, Amount: 0,
	})
	require.ErrorIs(t, err, ErrInvalidSavingsPreference)
}

func TestDivertSavingsForPayrollItem_FixedPercent(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()
	_, err := svc.SetSavingsPreference(context.Background(), orgID, employeeID, SavingsPreferenceUpdate{
		Enabled: true, Mode: models.SavingsFixedPercent, Amount: 10,
	})
	require.NoError(t, err)

	var diverted money.Kobo
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		var err error
		diverted, err = svc.DivertSavingsForPayrollItem(tx, orgID, employeeID, "ITEM-SAVINGS-1", money.FromNaira(100_000))
		return err
	}))
	assert.Equal(t, money.FromNaira(10_000), diverted, "10% of ₦100,000 net pay")

	balance, err := svc.GetSavingsBalance(context.Background(), orgID, employeeID)
	require.NoError(t, err)
	assert.Equal(t, money.FromNaira(10_000), balance)
}

func TestDivertSavingsForPayrollItem_RoundUp(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()
	_, err := svc.SetSavingsPreference(context.Background(), orgID, employeeID, SavingsPreferenceUpdate{
		Enabled: true, Mode: models.SavingsRoundUp, Amount: int64(money.FromNaira(1_000)),
	})
	require.NoError(t, err)

	// ₦99,700 rounds up to the next ₦1,000 — a ₦300 diversion.
	var diverted money.Kobo
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		var err error
		diverted, err = svc.DivertSavingsForPayrollItem(tx, orgID, employeeID, "ITEM-ROUNDUP-1",
			money.FromNaira(99_700))
		return err
	}))
	assert.Equal(t, money.FromNaira(300), diverted)
}

func TestDivertSavingsForPayrollItem_RoundUpOnExactMultipleSavesNothing(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()
	_, err := svc.SetSavingsPreference(context.Background(), orgID, employeeID, SavingsPreferenceUpdate{
		Enabled: true, Mode: models.SavingsRoundUp, Amount: int64(money.FromNaira(1_000)),
	})
	require.NoError(t, err)

	var diverted money.Kobo
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		var err error
		diverted, err = svc.DivertSavingsForPayrollItem(tx, orgID, employeeID, "ITEM-ROUNDUP-2",
			money.FromNaira(100_000))
		return err
	}))
	assert.Equal(t, money.Zero, diverted, "pay already lands on the round unit — nothing to divert")
}

// A round-up unit far larger than net pay must never consume the whole
// paycheck — the same non-negotiable that caps fixed_percent below 100.
func TestDivertSavingsForPayrollItem_RoundUpNeverExceedsNetPay(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()
	_, err := svc.SetSavingsPreference(context.Background(), orgID, employeeID, SavingsPreferenceUpdate{
		Enabled: true, Mode: models.SavingsRoundUp, Amount: int64(money.FromNaira(50_000)),
	})
	require.NoError(t, err)

	var diverted money.Kobo
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		var err error
		diverted, err = svc.DivertSavingsForPayrollItem(tx, orgID, employeeID, "ITEM-ROUNDUP-3",
			money.FromNaira(1_000)) // tiny net pay, unit dwarfs it
		return err
	}))
	assert.Equal(t, money.Zero, diverted, "a round-up delta that would consume the whole paycheck must divert nothing")
}

func TestDivertSavingsForPayrollItem_DisabledDivertsNothing(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	var diverted money.Kobo
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		var err error
		diverted, err = svc.DivertSavingsForPayrollItem(tx, orgID, employeeID, "ITEM-DISABLED-1", money.FromNaira(100_000))
		return err
	}))
	assert.Equal(t, money.Zero, diverted, "a worker who never opted in must never have money diverted")
}

// End to end: a salaried employee's payroll run must actually shrink the
// disbursed amount by their elected savings share, and the balance must land
// where GetSavingsBalance can see it.
func TestCreatePayroll_DivertsSavingsFromNetPay(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	ewaSvc := NewEWAService()
	_, err := ewaSvc.SetSavingsPreference(context.Background(), orgID, employeeID, SavingsPreferenceUpdate{
		Enabled: true, Mode: models.SavingsFixedPercent, Amount: 20,
	})
	require.NoError(t, err)

	period := time.Now().Format(PeriodLayout)
	payrollSvc := NewPayrollService(
		repository.NewPayrollRepository(models.DB),
		repository.NewEmployeeRepository(models.DB),
	)
	payroll, err := payrollSvc.CreatePayroll(context.Background(), orgID, period)
	require.NoError(t, err)

	var item models.PayrollItem
	require.NoError(t, models.DB.Where("payroll_id = ? AND employee_id = ?", payroll.ID, employeeID).
		First(&item).Error)
	assert.Equal(t, money.FromNaira(240_000), item.Amount, "80% of the ₦300,000 salary is actually disbursed")

	balance, err := ewaSvc.GetSavingsBalance(context.Background(), orgID, employeeID)
	require.NoError(t, err)
	assert.Equal(t, money.FromNaira(60_000), balance, "the other 20% landed in savings")
}
