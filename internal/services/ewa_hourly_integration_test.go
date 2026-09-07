//go:build integration

package services

import (
	"context"
	"testing"
	"time"

	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Only approved timesheet entries may fund EWA eligibility — see migration
// 000018. This is the hourly counterpart to the salaried straight-line tests
// in ewa_accrual_test.go, but must run against a real DB because the
// accrual now depends on rows in time_entries rather than a pure formula.
func TestGetEligibility_HourlyEmployee_OnlyCountsApprovedEntries(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_000)) // ₦1,000/hr
	teSvc := NewTimeEntryService()
	ewaSvc := NewEWAService()

	now := time.Now()

	// 8h approved, 8h still pending — only the approved hours must accrue.
	approved, err := teSvc.SubmitTimeEntry(context.Background(), orgID, employeeID, now, 480, "")
	require.NoError(t, err)
	_, err = teSvc.ApproveTimeEntry(context.Background(), orgID, approved.ID, "admin", "127.0.0.1")
	require.NoError(t, err)

	_, err = teSvc.SubmitTimeEntry(context.Background(), orgID, employeeID, now, 480, "still pending")
	require.NoError(t, err)

	el, err := ewaSvc.GetEligibility(context.Background(), orgID, employeeID, now)
	require.NoError(t, err)
	assert.False(t, el.Blocked)
	assert.Equal(t, money.FromNaira(8_000), el.AccruedToDate,
		"only the approved 8h should accrue, not the pending 8h")
}

// A rejected entry must never count, even after having once been pending.
func TestGetEligibility_HourlyEmployee_RejectedEntriesDoNotCount(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_000))
	teSvc := NewTimeEntryService()
	ewaSvc := NewEWAService()
	now := time.Now()

	entry, err := teSvc.SubmitTimeEntry(context.Background(), orgID, employeeID, now, 480, "")
	require.NoError(t, err)
	_, err = teSvc.RejectTimeEntry(context.Background(), orgID, entry.ID, "admin", "127.0.0.1", "duplicate entry")
	require.NoError(t, err)

	el, err := ewaSvc.GetEligibility(context.Background(), orgID, employeeID, now)
	require.NoError(t, err)
	assert.Equal(t, money.Zero, el.AccruedToDate)
}

// An hourly employee with no rate on file cannot be evaluated for eligibility
// at all — there is nothing to multiply approved hours by.
func TestGetEligibility_HourlyEmployee_NoRateIsBlocked(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, 0)
	ewaSvc := NewEWAService()

	el, err := ewaSvc.GetEligibility(context.Background(), orgID, employeeID, time.Now())
	require.NoError(t, err)
	assert.True(t, el.Blocked)
	assert.Equal(t, DeclineNoSalaryOnFile, el.BlockedReason)
}

// An entry approved for a different period must not leak into this period's
// accrual — the period boundary, not just the approval state, gates the sum.
func TestGetEligibility_HourlyEmployee_PriorPeriodEntriesExcluded(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_000))
	teSvc := NewTimeEntryService()
	ewaSvc := NewEWAService()

	now := time.Now()
	lastMonth := now.AddDate(0, -1, 0)
	// Guard against a run on the 1st/2nd where "last month" and "this month"
	// could collide after AddDate normalization.
	if lastMonth.Format(PeriodLayout) == now.Format(PeriodLayout) {
		t.Skip("test anchor date too close to a month boundary")
	}

	entry, err := teSvc.SubmitTimeEntry(context.Background(), orgID, employeeID, lastMonth, 480, "")
	require.NoError(t, err)
	_, err = teSvc.ApproveTimeEntry(context.Background(), orgID, entry.ID, "admin", "127.0.0.1")
	require.NoError(t, err)

	el, err := ewaSvc.GetEligibility(context.Background(), orgID, employeeID, now)
	require.NoError(t, err)
	assert.Equal(t, money.Zero, el.AccruedToDate,
		"last period's approved hours must not accrue toward this period")
}
