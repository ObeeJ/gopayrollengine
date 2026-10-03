package models

import (
	"testing"
	"time"

	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
)

func TestItemReference_FirstAttemptIsTheBareID(t *testing.T) {
	assert.Equal(t, "ITEM-ab12cd34", PayrollItem{ID: "ITEM-ab12cd34", Attempt: 1}.ItemReference())
	assert.Equal(t, "ITEM-ab12cd34", PayrollItem{ID: "ITEM-ab12cd34"}.ItemReference(), "zero value (pre-migration row) is attempt 1")
}

func TestItemReference_RetriesGetANewReference(t *testing.T) {
	assert.Equal(t, "ITEM-ab12cd34-R2", PayrollItem{ID: "ITEM-ab12cd34", Attempt: 2}.ItemReference())
	assert.Equal(t, "ITEM-ab12cd34-R7", PayrollItem{ID: "ITEM-ab12cd34", Attempt: 7}.ItemReference())
}

func TestParseItemReference_RoundTrips(t *testing.T) {
	for attempt := 1; attempt <= 5; attempt++ {
		item := PayrollItem{ID: "ITEM-ab12cd34", Attempt: attempt}
		id, got, ok := ParseItemReference(item.ItemReference())
		assert.True(t, ok)
		assert.Equal(t, "ITEM-ab12cd34", id)
		assert.Equal(t, attempt, got)
	}
}

func TestParseItemReference_RejectsEverythingElse(t *testing.T) {
	for _, ref := range []string{
		"", "EWA-ab12cd34", "GRANT-ab12cd34", "ITEM-", "item-ab12cd34",
		"ITEM-ab12cd34-R1", // attempt 1 is never written with a suffix
		"ITEM-ab12cd34-R0",
		"ITEM-ab12cd34-R",
		"ITEM-ab12cd34-R2-R3",
		"ITEM-ab12cd34-R-2",
		"ITEM-ab12cd34 ",
		"ITEM-ab12cd34'; DROP TABLE payroll_items;--",
		"ITEM-ab12cd34-R99999999999999999999", // overflows int
	} {
		_, _, ok := ParseItemReference(ref)
		assert.False(t, ok, "%q must not parse", ref)
	}
}

func item(status PayrollStatus, amount int64, sentAgo time.Duration, now time.Time) PayrollItem {
	it := PayrollItem{ID: "ITEM-x", Status: status, Amount: money.Kobo(amount)}
	if sentAgo > 0 {
		t := now.Add(-sentAgo)
		it.SentAt = &t
	}
	return it
}

func TestSummarizePayroll_Outcomes(t *testing.T) {
	now := time.Now()
	fresh := time.Minute
	cases := []struct {
		name    string
		batch   PayrollStatus
		items   []PayrollItem
		outcome PayrollOutcome
		paid    int
		failed  int
		paidK   int64
		failedK int64
		stuck   bool
	}{
		{"just created", PayrollPending,
			[]PayrollItem{item(PayrollPending, 100, 0, now)}, OutcomePending, 0, 0, 0, 0, false},
		{"handed to the bank, callbacks pending", PayrollProcessing,
			[]PayrollItem{item(PayrollProcessing, 100, fresh, now), item(PayrollProcessing, 200, fresh, now)},
			OutcomeInProgress, 0, 0, 0, 0, false},
		{"callbacks overdue", PayrollProcessing,
			[]PayrollItem{item(PayrollProcessing, 100, StuckAfter+time.Minute, now), item(PayrollCompleted, 200, 0, now)},
			OutcomeInProgress, 1, 0, 200, 0, true},
		{"everyone paid", PayrollCompleted,
			[]PayrollItem{item(PayrollCompleted, 100, 0, now), item(PayrollCompleted, 200, 0, now)},
			OutcomePaid, 2, 0, 300, 0, false},
		{"two paid one bounced is partially paid, not 'failed'", PayrollFailed,
			[]PayrollItem{item(PayrollCompleted, 100, 0, now), item(PayrollCompleted, 200, 0, now), item(PayrollFailed, 50, 0, now)},
			OutcomePartiallyPaid, 2, 1, 300, 50, false},
		{"nobody paid", PayrollFailed,
			[]PayrollItem{item(PayrollFailed, 100, 0, now), item(PayrollFailed, 200, 0, now)},
			OutcomeFailed, 0, 2, 0, 300, false},
		{"a zero-net line settled at creation is not a payment", PayrollFailed,
			[]PayrollItem{item(PayrollCompleted, 0, 0, now), item(PayrollFailed, 100, 0, now)},
			OutcomeFailed, 1, 1, 0, 100, false},
		{"retry in flight after a partial payment", PayrollProcessing,
			[]PayrollItem{item(PayrollCompleted, 100, 0, now), item(PayrollProcessing, 50, fresh, now)},
			OutcomeInProgress, 1, 0, 100, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SummarizePayroll(Payroll{Status: tc.batch}, tc.items, now)
			assert.Equal(t, tc.outcome, got.Outcome)
			assert.Equal(t, tc.paid, got.Paid)
			assert.Equal(t, tc.failed, got.Failed)
			assert.Equal(t, money.Kobo(tc.paidK), got.PaidKobo)
			assert.Equal(t, money.Kobo(tc.failedK), got.FailedKobo)
			assert.Equal(t, tc.stuck, got.Stuck)
		})
	}
}

func TestSummarizePayroll_StuckOnlyCountsItemsWithTheBank(t *testing.T) {
	now := time.Now()
	old := item(PayrollPending, 100, 0, now) // never sent: no sent_at, so it can't be "overdue"
	got := SummarizePayroll(Payroll{Status: PayrollProcessing}, []PayrollItem{old}, now)
	assert.False(t, got.Stuck)
	assert.Equal(t, 1, got.Pending)
}

// Items marked processing before sent_at existed have no SentAt; their last
// update is the fallback, so they are not invisible to stuck detection.
func TestSummarizePayroll_StuckFallsBackToUpdatedAtWhenSentAtIsMissing(t *testing.T) {
	now := time.Now()
	old := PayrollItem{Status: PayrollProcessing, UpdatedAt: now.Add(-StuckAfter - time.Hour)}
	fresh := PayrollItem{Status: PayrollProcessing, UpdatedAt: now.Add(-time.Minute)}
	assert.True(t, SummarizePayroll(Payroll{Status: PayrollProcessing}, []PayrollItem{old}, now).Stuck)
	assert.False(t, SummarizePayroll(Payroll{Status: PayrollProcessing}, []PayrollItem{fresh}, now).Stuck)
}
