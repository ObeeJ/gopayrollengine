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
)

// A payroll run must pay an hourly employee for real approved hours, not the
// unused Salary column — and must never pay for hours still pending review.
func TestCreatePayroll_HourlyEmployee_GrossFromApprovedMinutesOnly(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(2_000)) // ₦2,000/hr
	teSvc := NewTimeEntryService()
	now := time.Now()
	period := now.Format(PeriodLayout)

	approved, err := teSvc.SubmitTimeEntry(context.Background(), orgID, employeeID, now, 600, "") // 10h
	require.NoError(t, err)
	_, err = teSvc.ApproveTimeEntry(context.Background(), orgID, approved.ID, "admin", "127.0.0.1")
	require.NoError(t, err)

	// Still pending at payroll time — must not be paid for.
	_, err = teSvc.SubmitTimeEntry(context.Background(), orgID, employeeID, now, 120, "")
	require.NoError(t, err)

	payrollSvc := NewPayrollService(
		repository.NewPayrollRepository(models.DB),
		repository.NewEmployeeRepository(models.DB),
	)
	payroll, err := payrollSvc.CreatePayroll(context.Background(), orgID, period)
	require.NoError(t, err)

	var item models.PayrollItem
	require.NoError(t, models.DB.Where("payroll_id = ? AND employee_id = ?", payroll.ID, employeeID).
		First(&item).Error)
	assert.Equal(t, money.FromNaira(20_000), item.Amount, "10 approved hours at ₦2,000/hr, not the 2 pending")
	assert.Equal(t, money.FromNaira(20_000), payroll.TotalAmount)
}

// An hourly employee with no approved hours this period is still payable —
// zero, not an error — the same way a salaried employee always has an item.
func TestCreatePayroll_HourlyEmployee_NoApprovedHoursPaysZero(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(2_000))
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
	assert.Equal(t, money.Zero, item.Amount)
}
