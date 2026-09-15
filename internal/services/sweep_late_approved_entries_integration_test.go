//go:build integration

package services

import (
	"context"
	"testing"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newPayrollSvc() *PayrollService {
	return NewPayrollService(
		repository.NewPayrollRepository(models.DB),
		repository.NewEmployeeRepository(models.DB),
	)
}

// An entry approved only AFTER its own period's payroll already ran must not
// be lost — the next run that comes along has to sweep it up.
func TestCreatePayroll_LateApprovedEntry_SweptIntoNextRun(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_000))
	teSvc := NewTimeEntryService()
	payrollSvc := newPayrollSvc()

	// Jan: one entry submitted AND approved before payroll runs — pays normally.
	submitAndApprove(t, teSvc, orgID, employeeID, jan2024(2), 240, models.ShiftRegular)
	janPayroll, err := payrollSvc.CreatePayroll(context.Background(), orgID, jan2024Period)
	require.NoError(t, err)

	var janItem models.PayrollItem
	require.NoError(t, models.DB.Where("payroll_id = ? AND employee_id = ?", janPayroll.ID, employeeID).First(&janItem).Error)
	assert.Equal(t, money.FromNaira(4_000), janItem.Amount, "4h at ₦1,000/hr")

	// A second Jan entry is submitted before Jan payroll ran but only
	// APPROVED after — it must have been excluded from janItem above.
	late, err := teSvc.SubmitTimeEntry(context.Background(), orgID, employeeID, jan2024(3), 180, models.ShiftRegular, "")
	require.NoError(t, err)
	// Approve it now, simulating an admin reviewing it after Jan payroll already ran.
	_, err = teSvc.ApproveTimeEntry(context.Background(), orgID, late.ID, "admin", "127.0.0.1")
	require.NoError(t, err)

	// Feb payroll runs — must pick up the late Jan entry, even though its
	// work_date falls outside February.
	febPeriod := "2024-02"
	febPayroll, err := payrollSvc.CreatePayroll(context.Background(), orgID, febPeriod)
	require.NoError(t, err)

	var febItem models.PayrollItem
	require.NoError(t, models.DB.Where("payroll_id = ? AND employee_id = ?", febPayroll.ID, employeeID).First(&febItem).Error)
	assert.Equal(t, money.FromNaira(3_000), febItem.Amount, "3h at ₦1,000/hr, swept from the late Jan approval")

	var reloaded models.TimeEntry
	require.NoError(t, models.DB.First(&reloaded, "id = ?", late.ID).Error)
	require.NotNil(t, reloaded.PaidPayrollItemID)
	assert.Equal(t, febItem.ID, *reloaded.PaidPayrollItemID)
}

// A run must never pay an entry twice: an entry already paid by a previous
// run's item must be excluded from every later run.
func TestCreatePayroll_AlreadyPaidEntry_NeverPaidAgain(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_000))
	teSvc := NewTimeEntryService()
	payrollSvc := newPayrollSvc()

	submitAndApprove(t, teSvc, orgID, employeeID, jan2024(2), 240, models.ShiftRegular)
	_, err := payrollSvc.CreatePayroll(context.Background(), orgID, jan2024Period)
	require.NoError(t, err)

	// Feb payroll, with no new entries — must pay zero, not re-pay January's.
	febPayroll, err := payrollSvc.CreatePayroll(context.Background(), orgID, "2024-02")
	require.NoError(t, err)

	var febItem models.PayrollItem
	require.NoError(t, models.DB.Where("payroll_id = ? AND employee_id = ?", febPayroll.ID, employeeID).First(&febItem).Error)
	assert.Equal(t, money.Zero, febItem.Amount)
}

// The subtle correctness case: a late-swept entry's overtime status must
// account for minutes THAT SAME ISO WEEK already paid by an earlier run —
// otherwise a late approval could be underpaid as regular time even though
// the week's overtime threshold was already crossed by what was paid before.
func TestCreatePayroll_LateApprovedEntry_StillRatedAsOvertimeIfWeekAlreadyCrossedThreshold(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_000))
	teSvc := NewTimeEntryService()
	payrollSvc := newPayrollSvc()

	// Jan 1-5, 2024 is ISO week 1. Mon-Fri, 480min/day = 2400min — exactly
	// the default weekly threshold, all regular, all approved and paid by
	// January's run.
	for day := 1; day <= 5; day++ {
		submitAndApprove(t, teSvc, orgID, employeeID, jan2024(day), 480, models.ShiftRegular)
	}
	janPayroll, err := payrollSvc.CreatePayroll(context.Background(), orgID, jan2024Period)
	require.NoError(t, err)
	var janItem models.PayrollItem
	require.NoError(t, models.DB.Where("payroll_id = ? AND employee_id = ?", janPayroll.ID, employeeID).First(&janItem).Error)
	assert.Equal(t, money.FromNaira(40_000), janItem.Amount, "2400min at ₦1,000/hr, no overtime yet")

	// A sixth entry for the SAME week (Jan 6, still week 1) is approved only
	// after Jan payroll ran. Every one of its minutes is overtime: the
	// week's 2400min threshold was already fully used by the paid entries.
	late, err := teSvc.SubmitTimeEntry(context.Background(), orgID, employeeID, jan2024(6), 120, models.ShiftRegular, "")
	require.NoError(t, err)
	_, err = teSvc.ApproveTimeEntry(context.Background(), orgID, late.ID, "admin", "127.0.0.1")
	require.NoError(t, err)

	febPayroll, err := payrollSvc.CreatePayroll(context.Background(), orgID, "2024-02")
	require.NoError(t, err)
	var febItem models.PayrollItem
	require.NoError(t, models.DB.Where("payroll_id = ? AND employee_id = ?", febPayroll.ID, employeeID).First(&febItem).Error)
	// Base: 120min at flat rate = 1,000 × 120/60 = 2,000.
	// Premium: all 120min are overtime (extra 0.5x) = 1,000 × 120/60 × 0.5 = 1,000.
	assert.Equal(t, money.FromNaira(3_000), febItem.Amount,
		"120min entirely overtime — the week's threshold was already used up by January's paid entries")
}
