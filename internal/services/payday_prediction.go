package services

import (
	"errors"
	"sort"
	"time"

	"go-payroll-engine/internal/integrations/banklink"
	"go-payroll-engine/pkg/money"
)

// ErrInsufficientPaydayHistory means the transaction history handed in
// doesn't show a recurring income pattern confident enough to predict a
// payday from. Callers must treat this as "cannot predict" — never fall
// back to a guess. A direct debit collection scheduled against a bad guess
// is money pulled from a worker's account on a day nothing is actually
// there.
var ErrInsufficientPaydayHistory = errors.New("payday prediction: no confident recurring income pattern")

// PaydayConfidence reflects how many times the pattern has actually
// repeated — never a statement about how large or reliable the underlying
// income itself is.
type PaydayConfidence string

const (
	ConfidenceLow    PaydayConfidence = "low"    // 2 occurrences — a pattern, barely
	ConfidenceMedium PaydayConfidence = "medium" // 3 occurrences
	ConfidenceHigh   PaydayConfidence = "high"   // 4+ occurrences
)

// PaydayPrediction is PredictNextPayday's result: when the worker's next
// recurring deposit is expected, and the evidence behind that estimate.
type PaydayPrediction struct {
	NextPredictedDate time.Time
	IntervalDays      int
	AverageAmountKobo money.Kobo
	OccurrencesSeen   int
	Confidence        PaydayConfidence
}

// amountClusterTolerance is how close two deposits' amounts must be
// (relative to the larger) to be considered the same recurring payment.
// Real paychecks repeat almost exactly; 10% comfortably covers small
// variance (overtime, a rounded deduction) without folding in unrelated
// transfers.
const amountClusterTolerance = 0.10

// intervalConsistencyTolerance bounds how far any single gap between
// occurrences may drift from the cluster's mean gap, as a fraction of that
// mean. Real pay cadences (weekly/biweekly/monthly) are far more regular
// than this; a looser pattern is more likely coincidence than income.
const intervalConsistencyTolerance = 0.20

// minIntervalDays / maxIntervalDays bound what counts as a payday cadence at
// all — narrower than weekly or wider than monthly is out of scope for a
// v1 direct-debit collection date, not because it can't be income, but
// because collecting against it needs more confidence than this function
// can offer from bank history alone.
const (
	minIntervalDays = 5
	maxIntervalDays = 35
)

// PredictNextPayday looks for a recurring, similarly-sized credit in
// transactions and predicts when it will next occur. Returns
// ErrInsufficientPaydayHistory if nothing in the history clears the bar —
// there is no partial-confidence prediction; a caller either gets a payday
// to collect against or nothing at all.
//
// Only Credit transactions dated on or before asOf are considered. The
// algorithm:
//  1. Clusters credits by amount similarity (amountClusterTolerance) — a
//     recurring paycheck repeats at nearly the same amount far more
//     reliably than at a perfectly regular calendar date (a salary paid on
//     the last business day of the month moves around; its amount usually
//     doesn't).
//  2. Takes the largest such cluster with at least 2 occurrences.
//  3. Requires the gaps between consecutive occurrences in that cluster to
//     be mutually consistent (intervalConsistencyTolerance) and within a
//     plausible pay cadence (minIntervalDays..maxIntervalDays) — a large
//     cluster of similarly-sized but irregularly-timed deposits is not a
//     payday pattern.
func PredictNextPayday(transactions []banklink.Transaction, asOf time.Time) (*PaydayPrediction, error) {
	var credits []banklink.Transaction
	for _, tx := range transactions {
		if tx.Direction == banklink.Credit && !tx.Date.After(asOf) && tx.Amount.IsPositive() {
			credits = append(credits, tx)
		}
	}
	if len(credits) < 2 {
		return nil, ErrInsufficientPaydayHistory
	}

	cluster := largestAmountCluster(credits)
	if len(cluster) < 2 {
		return nil, ErrInsufficientPaydayHistory
	}

	sort.Slice(cluster, func(i, j int) bool { return cluster[i].Date.Before(cluster[j].Date) })

	intervals := make([]float64, 0, len(cluster)-1)
	for i := 1; i < len(cluster); i++ {
		days := cluster[i].Date.Sub(cluster[i-1].Date).Hours() / 24
		intervals = append(intervals, days)
	}

	meanInterval := mean(intervals)
	if meanInterval < minIntervalDays || meanInterval > maxIntervalDays {
		return nil, ErrInsufficientPaydayHistory
	}
	for _, gap := range intervals {
		if relativeDiff(gap, meanInterval) > intervalConsistencyTolerance {
			return nil, ErrInsufficientPaydayHistory
		}
	}

	amounts := make([]money.Kobo, len(cluster))
	for i, tx := range cluster {
		amounts[i] = tx.Amount
	}
	amountSum, err := money.Sum(amounts)
	if err != nil {
		return nil, err
	}
	avgAmount := money.Kobo(int64(amountSum) / int64(len(cluster)))

	roundedInterval := int(meanInterval + 0.5)
	lastDate := cluster[len(cluster)-1].Date
	nextDate := lastDate.AddDate(0, 0, roundedInterval)

	confidence := ConfidenceLow
	switch {
	case len(cluster) >= 4:
		confidence = ConfidenceHigh
	case len(cluster) == 3:
		confidence = ConfidenceMedium
	}

	return &PaydayPrediction{
		NextPredictedDate: nextDate,
		IntervalDays:      roundedInterval,
		AverageAmountKobo: avgAmount,
		OccurrencesSeen:   len(cluster),
		Confidence:        confidence,
	}, nil
}

// largestAmountCluster groups transactions whose amounts are mutually close
// (a chain where each item is within amountClusterTolerance of its
// amount-sorted neighbor) and returns the largest such group. Clustering by
// sorted-neighbor distance rather than all-pairs comparison is O(n log n)
// and is exactly the right shape for what recurring pay actually looks
// like: one tight, unimodal group of near-identical amounts standing out
// from everything else in the history.
func largestAmountCluster(credits []banklink.Transaction) []banklink.Transaction {
	sorted := make([]banklink.Transaction, len(credits))
	copy(sorted, credits)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Amount < sorted[j].Amount })

	var clusters [][]banklink.Transaction
	var current []banklink.Transaction
	for _, tx := range sorted {
		if len(current) > 0 {
			last := current[len(current)-1]
			if relativeDiff(float64(tx.Amount), float64(last.Amount)) > amountClusterTolerance {
				clusters = append(clusters, current)
				current = nil
			}
		}
		current = append(current, tx)
	}
	if len(current) > 0 {
		clusters = append(clusters, current)
	}

	var best []banklink.Transaction
	for _, c := range clusters {
		if len(c) > len(best) {
			best = c
		}
	}
	return best
}

func relativeDiff(a, b float64) float64 {
	if a == 0 && b == 0 {
		return 0
	}
	diff := a - b
	if diff < 0 {
		diff = -diff
	}
	denom := a
	if b > denom {
		denom = b
	}
	return diff / denom
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	var sum float64
	for _, v := range values {
		sum += v
	}
	return sum / float64(len(values))
}
