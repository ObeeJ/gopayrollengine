package models

import (
	"fmt"
	"regexp"
	"strconv"
	"time"

	"go-payroll-engine/pkg/money"
)

// ItemReference is the reference a payroll item is sent to the bank under.
//
// Attempt 1 is the bare item ID — every existing row, every webhook already
// in flight, and every test keeps working. A retry (attempt N>1) is sent as
// "<item id>-R<N>": a provider may treat the reference as an idempotency key
// and answer a re-submission of a *failed* reference with the failed result,
// so a retry under the old reference might never actually run.
func (i PayrollItem) ItemReference() string {
	if i.Attempt <= 1 {
		return i.ID
	}
	return fmt.Sprintf("%s-R%d", i.ID, i.Attempt)
}

var itemReferencePattern = regexp.MustCompile(`^(ITEM-[0-9A-Za-z]+)(?:-R([0-9]+))?$`)

// ParseItemReference splits a callback's reference into the item ID and the
// attempt it belongs to. ok is false for anything that is not a payroll item
// reference.
func ParseItemReference(ref string) (itemID string, attempt int, ok bool) {
	m := itemReferencePattern.FindStringSubmatch(ref)
	if m == nil {
		return "", 0, false
	}
	attempt = 1
	if m[2] != "" {
		n, err := strconv.Atoi(m[2])
		if err != nil || n < 2 {
			return "", 0, false
		}
		attempt = n
	}
	return m[1], attempt, true
}

// PayrollOutcome is the batch's result as a person would describe it. The
// stored status is a state machine and overloads "failed": a batch where two
// people were paid and one payout bounced is 'failed', which reads as though
// nobody was paid.
type PayrollOutcome string

const (
	OutcomePending       PayrollOutcome = "pending"
	OutcomeInProgress    PayrollOutcome = "in_progress"
	OutcomePaid          PayrollOutcome = "paid"
	OutcomePartiallyPaid PayrollOutcome = "partially_paid"
	OutcomeFailed        PayrollOutcome = "failed"
)

// PayrollSummary counts a batch's items by state.
type PayrollSummary struct {
	Outcome    PayrollOutcome `json:"outcome"`
	Paid       int            `json:"paid_items"`
	Failed     int            `json:"failed_items"`
	Processing int            `json:"processing_items"`
	Pending    int            `json:"pending_items"`
	PaidKobo   money.Kobo     `json:"paid_kobo"`
	FailedKobo money.Kobo     `json:"failed_kobo"`
	// Stuck is true when an item has been with the bank longer than any
	// callback should take; see StuckAfter.
	Stuck bool `json:"stuck"`
}

// StuckAfter is how long an item may be with the bank before its missing
// callback is treated as a problem to chase. Provider callbacks normally land
// within minutes; two hours is long past any retry schedule worth waiting on.
// Overridable at startup (PAYROLL_STUCK_AFTER) so the live end-to-end test
// doesn't have to wait two hours.
var StuckAfter = 2 * time.Hour

// SummarizePayroll derives the batch's human outcome and counts from its items.
// Pure: no I/O, so every branch is unit-tested.
func SummarizePayroll(p Payroll, items []PayrollItem, now time.Time) PayrollSummary {
	var s PayrollSummary
	var paid, failed []money.Kobo
	for _, it := range items {
		switch it.Status {
		case PayrollCompleted:
			s.Paid++
			paid = append(paid, it.Amount)
		case PayrollFailed:
			s.Failed++
			failed = append(failed, it.Amount)
		case PayrollProcessing:
			s.Processing++
			// sent_at is NULL on items marked processing before migration 000034;
			// their last update is the best available "since".
			since := it.UpdatedAt
			if it.SentAt != nil {
				since = *it.SentAt
			}
			if now.Sub(since) > StuckAfter {
				s.Stuck = true
			}
		default:
			s.Pending++
		}
	}
	// A sum can only overflow int64 for absurd totals; the batch total was
	// overflow-checked when it was built, so a failure here means corrupt data
	// and zero is the honest "unknown" for a display figure.
	s.PaidKobo, _ = money.Sum(paid)
	s.FailedKobo, _ = money.Sum(failed)

	switch p.Status {
	case PayrollPending:
		s.Outcome = OutcomePending
	case PayrollProcessing:
		s.Outcome = OutcomeInProgress
	case PayrollCompleted:
		s.Outcome = OutcomePaid
	case PayrollFailed:
		if s.PaidKobo.IsPositive() {
			s.Outcome = OutcomePartiallyPaid
		} else {
			s.Outcome = OutcomeFailed
		}
	}
	return s
}
