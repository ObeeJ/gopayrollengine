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

// submitAndApprove is a shorthand for the submit-then-approve round trip
// these tests repeat for every entry: only approved minutes count toward
// ComputeHourlyGross (see migration 000018's invariant).
func submitAndApprove(
	t *testing.T, teSvc *TimeEntryService, orgID, employeeID string,
	workDate time.Time, minutes int, shiftType models.TimeEntryShiftType,
) {
	t.Helper()
	entry, err := teSvc.SubmitTimeEntry(context.Background(), orgID, employeeID, workDate, minutes, shiftType, "")
	require.NoError(t, err)
	_, err = teSvc.ApproveTimeEntry(context.Background(), orgID, entry.ID, "admin", "127.0.0.1")
	require.NoError(t, err)
}

// jan2024 builds a fixed date in January 2024 — ISO week 1 is Jan 1 (Mon)
// through Jan 7 (Sun), week 2 starts Jan 8 (Mon). Using a fixed historical
// month instead of "now" gives these tests deterministic control over ISO
// week boundaries regardless of what day they actually run.
func jan2024(day int) time.Time {
	return time.Date(2024, time.January, day, 0, 0, 0, 0, time.UTC)
}

const jan2024Period = "2024-01"

func TestComputeHourlyGross_FlatRateNoOvertimeNoShiftDiff(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_000))
	teSvc := NewTimeEntryService()

	submitAndApprove(t, teSvc, orgID, employeeID, jan2024(2), 240, models.ShiftRegular) // 4h

	breakdown, err := ComputeHourlyGross(models.DB, orgID, employeeID, jan2024Period, jan2024(31), money.FromNaira(1_000))
	require.NoError(t, err)
	assert.Equal(t, int64(240), breakdown.RegularMinutes)
	assert.Equal(t, int64(0), breakdown.OvertimeMinutes)
	assert.Equal(t, money.FromNaira(4_000), breakdown.BaseKobo)
	assert.Equal(t, money.Zero, breakdown.OvertimePremiumKobo)
	assert.Equal(t, money.FromNaira(4_000), breakdown.GrossKobo)
}

// A night shift is paid at the configured differential even when it never
// crosses the overtime threshold.
func TestComputeHourlyGross_ShiftDifferentialAppliesToBasePay(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_000))
	teSvc := NewTimeEntryService()
	payrollSvc := NewPayrollService(nil, nil)

	_, err := payrollSvc.UpdatePayrollPolicy(context.Background(), orgID, PayrollPolicyUpdate{
		NightShiftMultiplierBps: intPtr(12000), // 1.2x
	}, "127.0.0.1")
	require.NoError(t, err)

	submitAndApprove(t, teSvc, orgID, employeeID, jan2024(2), 240, models.ShiftNight) // 4h @ night rate

	breakdown, err := ComputeHourlyGross(models.DB, orgID, employeeID, jan2024Period, jan2024(31), money.FromNaira(1_000))
	require.NoError(t, err)
	assert.Equal(t, int64(240), breakdown.RegularMinutes)
	assert.Equal(t, int64(0), breakdown.OvertimeMinutes)
	assert.Equal(t, money.FromNaira(4_800), breakdown.BaseKobo, "4h at ₦1,000×1.2/hr")
	assert.Equal(t, money.Zero, breakdown.OvertimePremiumKobo)
	assert.Equal(t, money.FromNaira(4_800), breakdown.GrossKobo)
}

// An entry that straddles the weekly overtime threshold is split
// proportionally: the portion under the threshold is regular, the rest is
// overtime — both within the same single entry.
func TestComputeHourlyGross_OvertimeWithinSingleWeek(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_000))
	teSvc := NewTimeEntryService()

	// Mon-Thu: 480min/day = 1920min. Fri: 600min — before=1920, after=2520,
	// threshold=2400 (default), so 480min regular + 120min overtime.
	for day := 1; day <= 4; day++ {
		submitAndApprove(t, teSvc, orgID, employeeID, jan2024(day), 480, models.ShiftRegular)
	}
	submitAndApprove(t, teSvc, orgID, employeeID, jan2024(5), 600, models.ShiftRegular)

	breakdown, err := ComputeHourlyGross(models.DB, orgID, employeeID, jan2024Period, jan2024(31), money.FromNaira(1_000))
	require.NoError(t, err)
	assert.Equal(t, int64(2400), breakdown.RegularMinutes)
	assert.Equal(t, int64(120), breakdown.OvertimeMinutes)
	// Base: every minute (2520) at flat rate = 1000 × 2520/60 = 42,000.
	assert.Equal(t, money.FromNaira(42_000), breakdown.BaseKobo)
	// Premium: only the 120 OT minutes, at the extra 0.5x = 1000 × 120/60 × 0.5 = 1,000.
	assert.Equal(t, money.FromNaira(1_000), breakdown.OvertimePremiumKobo)
	assert.Equal(t, money.FromNaira(43_000), breakdown.GrossKobo)
}

// Crossing a week boundary resets the overtime counter — hours already paid
// as overtime in week 1 must not suppress or inflate week 2's count.
func TestComputeHourlyGross_OvertimeResetsAtWeekBoundary(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_000))
	teSvc := NewTimeEntryService()

	// Week 1 (Jan 1-7): four 500min days = 2000, fifth day 500min pushes to
	// 2500 — 400 regular + 100 overtime from that last entry.
	for day := 1; day <= 5; day++ {
		submitAndApprove(t, teSvc, orgID, employeeID, jan2024(day), 500, models.ShiftRegular)
	}
	// Week 2 (Jan 8-14): a single 200min entry — far under threshold, must
	// be entirely regular; a buggy running total that failed to reset on the
	// new ISO week would show this as (wrongly) overtime-adjacent.
	submitAndApprove(t, teSvc, orgID, employeeID, jan2024(8), 200, models.ShiftRegular)

	breakdown, err := ComputeHourlyGross(models.DB, orgID, employeeID, jan2024Period, jan2024(31), money.FromNaira(1_000))
	require.NoError(t, err)
	assert.Equal(t, int64(2600), breakdown.RegularMinutes, "2400 week-1 regular + 200 week-2 regular")
	assert.Equal(t, int64(100), breakdown.OvertimeMinutes, "only week 1's overflow")
}

// The defining "additive" behavior: an overtime hour on a differentiated
// shift earns BOTH the shift differential (on all its minutes, like any
// other hour on that shift) AND the overtime premium on top — never one
// instead of the other.
func TestComputeHourlyGross_OvertimePortionEarnsBothShiftDiffAndPremium(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_000))
	teSvc := NewTimeEntryService()
	payrollSvc := NewPayrollService(nil, nil)

	_, err := payrollSvc.UpdatePayrollPolicy(context.Background(), orgID, PayrollPolicyUpdate{
		NightShiftMultiplierBps: intPtr(12000), // 1.2x
	}, "127.0.0.1")
	require.NoError(t, err)

	// Mon-Thu: 480min/day night = 1920min. Fri: 600min night — before=1920,
	// after=2520 → 480 regular + 120 overtime, all on the night differential.
	for day := 1; day <= 4; day++ {
		submitAndApprove(t, teSvc, orgID, employeeID, jan2024(day), 480, models.ShiftNight)
	}
	submitAndApprove(t, teSvc, orgID, employeeID, jan2024(5), 600, models.ShiftNight)

	breakdown, err := ComputeHourlyGross(models.DB, orgID, employeeID, jan2024Period, jan2024(31), money.FromNaira(1_000))
	require.NoError(t, err)
	assert.Equal(t, int64(2400), breakdown.RegularMinutes)
	assert.Equal(t, int64(120), breakdown.OvertimeMinutes)
	// Base: every minute (2520) at the night rate = 1200 × 2520/60 = 50,400.
	assert.Equal(t, money.FromNaira(50_400), breakdown.BaseKobo)
	// Premium: 120 OT minutes at the night rate's extra 0.5x = 1200 × 120/60 × 0.5 = 1,200.
	assert.Equal(t, money.FromNaira(1_200), breakdown.OvertimePremiumKobo)
	assert.Equal(t, money.FromNaira(51_600), breakdown.GrossKobo)
}

// Unapproved (pending) minutes must not count toward gross, mirroring the
// same invariant SumApprovedMinutes already enforces for the flat-rate path.
func TestComputeHourlyGross_IgnoresUnapprovedEntries(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_000))
	teSvc := NewTimeEntryService()

	submitAndApprove(t, teSvc, orgID, employeeID, jan2024(2), 240, models.ShiftRegular)
	// Left pending — must not count.
	_, err := teSvc.SubmitTimeEntry(context.Background(), orgID, employeeID, jan2024(3), 480, models.ShiftRegular, "")
	require.NoError(t, err)

	breakdown, err := ComputeHourlyGross(models.DB, orgID, employeeID, jan2024Period, jan2024(31), money.FromNaira(1_000))
	require.NoError(t, err)
	assert.Equal(t, int64(240), breakdown.RegularMinutes)
	assert.Equal(t, money.FromNaira(4_000), breakdown.GrossKobo)
}

// Full end-to-end: CreatePayroll must produce a gross that reflects
// shift-differential + overtime, not the flat rate.
func TestCreatePayroll_HourlyEmployee_AppliesShiftDifferentialAndOvertime(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_000))
	teSvc := NewTimeEntryService()
	payrollSvc := NewPayrollService(
		repository.NewPayrollRepository(models.DB),
		repository.NewEmployeeRepository(models.DB),
	)

	_, err := payrollSvc.UpdatePayrollPolicy(context.Background(), orgID, PayrollPolicyUpdate{
		WeekendShiftMultiplierBps: intPtr(13000), // 1.3x
	}, "127.0.0.1")
	require.NoError(t, err)

	// Jan 6, 2024 is a Saturday — weekend shift, 300min, well under threshold.
	submitAndApprove(t, teSvc, orgID, employeeID, jan2024(6), 300, models.ShiftWeekend)

	payroll, err := payrollSvc.CreatePayroll(context.Background(), orgID, jan2024Period)
	require.NoError(t, err)

	var item models.PayrollItem
	require.NoError(t, models.DB.Where("payroll_id = ? AND employee_id = ?", payroll.ID, employeeID).
		First(&item).Error)
	// 300min = 5h at ₦1,000×1.3/hr = 6,500.
	assert.Equal(t, money.FromNaira(6_500), item.Amount)
}
