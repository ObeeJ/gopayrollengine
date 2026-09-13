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

func TestAddBill_ValidatesAndPersists(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	bill, err := svc.AddBill(context.Background(), orgID, employeeID, "Rent", money.FromNaira(80_000), 25)
	require.NoError(t, err)
	assert.Equal(t, "Rent", bill.Name)
	assert.True(t, bill.Active)

	bills, err := svc.ListBills(context.Background(), orgID, employeeID)
	require.NoError(t, err)
	require.Len(t, bills, 1)
	assert.Equal(t, bill.ID, bills[0].ID)
}

func TestAddBill_RejectsInvalidInput(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	_, err := svc.AddBill(context.Background(), orgID, employeeID, "", money.FromNaira(1_000), 5)
	require.ErrorIs(t, err, ErrInvalidBill, "empty name")

	_, err = svc.AddBill(context.Background(), orgID, employeeID, "Rent", 0, 5)
	require.ErrorIs(t, err, ErrInvalidBill, "non-positive amount")

	_, err = svc.AddBill(context.Background(), orgID, employeeID, "Rent", money.FromNaira(1_000), 32)
	require.ErrorIs(t, err, ErrInvalidBill, "due_day out of range")
}

func TestRemoveBill_DeactivatesAndDropsFromList(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	bill, err := svc.AddBill(context.Background(), orgID, employeeID, "Electricity", money.FromNaira(15_000), 10)
	require.NoError(t, err)

	require.NoError(t, svc.RemoveBill(context.Background(), orgID, employeeID, bill.ID))

	bills, err := svc.ListBills(context.Background(), orgID, employeeID)
	require.NoError(t, err)
	assert.Empty(t, bills)
}

func TestNextBillDueDate_ResolvesShortMonthAndRollover(t *testing.T) {
	// A due_day of 31 in February resolves to the 28th (or 29th), never a
	// different stored value — and once that date has passed, the next
	// occurrence rolls into the following month correctly.
	asOf := time.Date(2027, time.February, 1, 0, 0, 0, 0, time.UTC)
	due := nextBillDueDate(asOf, 31)
	assert.Equal(t, time.Date(2027, time.February, 28, 0, 0, 0, 0, time.UTC), due,
		"2027 is not a leap year")

	asOfAfter := time.Date(2027, time.February, 28, 0, 0, 0, 0, time.UTC)
	dueNext := nextBillDueDate(asOfAfter, 31)
	assert.Equal(t, time.Date(2027, time.February, 28, 0, 0, 0, 0, time.UTC), dueNext,
		"due today still counts as the current occurrence, not yet rolled over")

	asOfPassed := time.Date(2027, time.March, 1, 0, 0, 0, 0, time.UTC)
	dueRolled := nextBillDueDate(asOfPassed, 31)
	assert.Equal(t, time.Date(2027, time.March, 31, 0, 0, 0, 0, time.UTC), dueRolled)
}

func TestNextBillDueDate_RollsOverYearBoundary(t *testing.T) {
	asOf := time.Date(2027, time.December, 20, 0, 0, 0, 0, time.UTC)
	due := nextBillDueDate(asOf, 5)
	assert.Equal(t, time.Date(2028, time.January, 5, 0, 0, 0, 0, time.UTC), due)
}

// The defining case for bill-timing: a bill due before the next payday is a
// timing mismatch, and if the worker's available EWA draw covers it, the
// plan must say so precisely.
func TestGetBillTimingPlan_FlagsMismatchAndCoverability(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	// asOf mid-month: a bill due later this same month falls before payday
	// (month-end); a bill due next month does not.
	asOf := time.Date(2027, time.June, 5, 0, 0, 0, 0, time.UTC)

	mismatched, err := svc.AddBill(context.Background(), orgID, employeeID, "Rent", money.FromNaira(50_000), 15)
	require.NoError(t, err)
	// due_day 1 has already passed this month (asOf is June 5), so it rolls
	// to July 1 — on or after the June 30 payday, hence not a mismatch.
	notMismatched, err := svc.AddBill(context.Background(), orgID, employeeID, "Insurance", money.FromNaira(10_000), 1)
	require.NoError(t, err)

	plan, err := svc.GetBillTimingPlan(context.Background(), orgID, employeeID, asOf)
	require.NoError(t, err)

	require.Len(t, plan.Bills, 2)
	byID := map[string]BillTimingEntry{}
	for _, e := range plan.Bills {
		byID[e.Bill.ID] = e
	}

	rentEntry := byID[mismatched.ID]
	assert.True(t, rentEntry.Mismatched, "due June 15, before the June 30 payday")

	insuranceEntry := byID[notMismatched.ID]
	assert.False(t, insuranceEntry.Mismatched, "due_day 1 has already passed this month, so it rolls to July 1 — on/after the June 30 payday")

	require.True(t, plan.Available.IsPositive(), "a fresh salaried worker mid-period should have some availability")
	if plan.Available >= money.FromNaira(50_000) {
		assert.True(t, rentEntry.Coverable)
	}

	assert.Equal(t, money.FromNaira(50_000), plan.TotalDueBeforePayday,
		"only the mismatched bill counts toward the timing gap")
}

func TestGetBillTimingPlan_NoBillsIsEmptyNotError(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()

	plan, err := svc.GetBillTimingPlan(context.Background(), orgID, employeeID, time.Now())
	require.NoError(t, err)
	assert.Empty(t, plan.Bills)
	assert.Equal(t, money.Zero, plan.TotalDueBeforePayday)
}
