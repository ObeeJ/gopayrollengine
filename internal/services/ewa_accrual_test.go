package services

import (
	"testing"
	"time"

	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// August 2026: 1st is a Saturday, 31st is a Monday — 21 working days.
const augustWorkingDays = 21

func atUTC(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 9, 0, 0, 0, time.UTC)
}

func TestAccruedToDate_NothingEarnedOnDayOne(t *testing.T) {
	salary := money.FromNaira(210_000)

	// 1 Aug 2026 is a Saturday; no working day has completed.
	got, err := AccruedToDate(salary, "2026-08", atUTC(2026, time.August, 1))
	require.NoError(t, err)
	assert.Equal(t, money.Zero, got)
}

func TestAccruedToDate_MidMonthIsProRata(t *testing.T) {
	salary := money.FromNaira(210_000)

	// By the morning of Mon 17 Aug, the 10 working days of 3–14 Aug have completed.
	got, err := AccruedToDate(salary, "2026-08", atUTC(2026, time.August, 17))
	require.NoError(t, err)

	want, err := salary.Percent(10, augustWorkingDays)
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Less(t, int64(got), int64(salary), "mid-month accrual must be below full salary")
}

func TestAccruedToDate_FullSalaryAfterPeriodEnds(t *testing.T) {
	salary := money.FromNaira(210_000)

	got, err := AccruedToDate(salary, "2026-08", atUTC(2026, time.September, 1))
	require.NoError(t, err)
	assert.Equal(t, salary, got)
}

func TestAccruedToDate_ZeroBeforePeriodStarts(t *testing.T) {
	salary := money.FromNaira(210_000)

	got, err := AccruedToDate(salary, "2026-08", atUTC(2026, time.July, 20))
	require.NoError(t, err)
	assert.Equal(t, money.Zero, got)
}

func TestAccruedToDate_WeekendDoesNotAccrue(t *testing.T) {
	salary := money.FromNaira(210_000)

	// Fri 7 Aug close vs Sun 9 Aug: the weekend adds nothing.
	friday, err := AccruedToDate(salary, "2026-08", atUTC(2026, time.August, 8))
	require.NoError(t, err)
	sunday, err := AccruedToDate(salary, "2026-08", atUTC(2026, time.August, 9))
	require.NoError(t, err)

	assert.Equal(t, friday, sunday, "no wages accrue over a weekend")
}

func TestAccruedToDate_MonotonicAcrossMonth(t *testing.T) {
	salary := money.FromNaira(210_000)

	var prev money.Kobo
	for day := 1; day <= 31; day++ {
		got, err := AccruedToDate(salary, "2026-08", atUTC(2026, time.August, day))
		require.NoError(t, err)
		assert.GreaterOrEqual(t, int64(got), int64(prev),
			"accrual must never decrease (day %d)", day)
		assert.LessOrEqual(t, int64(got), int64(salary),
			"accrual must never exceed salary (day %d)", day)
		prev = got
	}
}

func TestAccruedToDate_ZeroSalary(t *testing.T) {
	got, err := AccruedToDate(money.Zero, "2026-08", atUTC(2026, time.August, 17))
	require.NoError(t, err)
	assert.Equal(t, money.Zero, got)
}

func TestAccruedToDate_InvalidPeriod(t *testing.T) {
	_, err := AccruedToDate(money.FromNaira(100_000), "August 2026", atUTC(2026, time.August, 17))
	assert.Error(t, err)
}

func TestWorkingDaysBetween_KnownMonth(t *testing.T) {
	start := time.Date(2026, time.August, 1, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, augustWorkingDays, workingDaysBetween(start, start.AddDate(0, 1, 0)))
}

// The core EWA invariant: what a worker may draw can never exceed what they have
// actually earned, at any point in the period, under any policy percentage.
func TestAccrual_CapNeverExceedsEarnings(t *testing.T) {
	salary := money.FromNaira(210_000)

	for day := 1; day <= 31; day++ {
		accrued, err := AccruedToDate(salary, "2026-08", atUTC(2026, time.August, day))
		require.NoError(t, err)

		for _, pct := range []int64{1, 25, 50, 75, 100} {
			cap, err := accrued.Percent(pct, 100)
			require.NoError(t, err)
			assert.LessOrEqual(t, int64(cap), int64(accrued),
				"day %d at %d%%: cap must not exceed accrued wages", day, pct)
		}
	}
}
