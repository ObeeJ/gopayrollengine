package services

import (
	"testing"
	"time"

	"go-payroll-engine/internal/integrations/banklink"
	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var predictionNow = time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)

// credit builds a Credit transaction daysAgo before predictionNow.
func credit(daysAgo int, amount money.Kobo) banklink.Transaction {
	return banklink.Transaction{
		Date:      predictionNow.AddDate(0, 0, -daysAgo),
		Amount:    amount,
		Direction: banklink.Credit,
	}
}

func debit(daysAgo int, amount money.Kobo) banklink.Transaction {
	return banklink.Transaction{
		Date:      predictionNow.AddDate(0, 0, -daysAgo),
		Amount:    amount,
		Direction: banklink.Debit,
	}
}

// recurringCredits builds n occurrences of amount, spaced intervalDays
// apart, most recent daysAgoLast before predictionNow — the shape of a real
// recurring paycheck.
func recurringCredits(n, intervalDays, daysAgoLast int, amount money.Kobo) []banklink.Transaction {
	txs := make([]banklink.Transaction, 0, n)
	for i := 0; i < n; i++ {
		txs = append(txs, credit(daysAgoLast+i*intervalDays, amount))
	}
	return txs
}

func TestPredictNextPayday_MonthlySalary_HighConfidence(t *testing.T) {
	txs := recurringCredits(4, 30, 1, money.FromNaira(300_000))
	pred, err := PredictNextPayday(txs, predictionNow)
	require.NoError(t, err)
	assert.Equal(t, ConfidenceHigh, pred.Confidence)
	assert.Equal(t, 4, pred.OccurrencesSeen)
	assert.Equal(t, 30, pred.IntervalDays)
	assert.Equal(t, money.FromNaira(300_000), pred.AverageAmountKobo)
	assert.Equal(t, predictionNow.AddDate(0, 0, -1+30), pred.NextPredictedDate)
}

func TestPredictNextPayday_Biweekly_MediumConfidence(t *testing.T) {
	txs := recurringCredits(3, 14, 2, money.FromNaira(150_000))
	pred, err := PredictNextPayday(txs, predictionNow)
	require.NoError(t, err)
	assert.Equal(t, ConfidenceMedium, pred.Confidence)
	assert.Equal(t, 3, pred.OccurrencesSeen)
	assert.Equal(t, 14, pred.IntervalDays)
}

func TestPredictNextPayday_Weekly_LowConfidence(t *testing.T) {
	txs := recurringCredits(2, 7, 3, money.FromNaira(50_000))
	pred, err := PredictNextPayday(txs, predictionNow)
	require.NoError(t, err)
	assert.Equal(t, ConfidenceLow, pred.Confidence)
	assert.Equal(t, 2, pred.OccurrencesSeen)
	assert.Equal(t, 7, pred.IntervalDays)
}

func TestPredictNextPayday_NoTransactions_Insufficient(t *testing.T) {
	_, err := PredictNextPayday(nil, predictionNow)
	assert.ErrorIs(t, err, ErrInsufficientPaydayHistory)
}

func TestPredictNextPayday_SingleCredit_Insufficient(t *testing.T) {
	txs := []banklink.Transaction{credit(5, money.FromNaira(300_000))}
	_, err := PredictNextPayday(txs, predictionNow)
	assert.ErrorIs(t, err, ErrInsufficientPaydayHistory)
}

func TestPredictNextPayday_AllDifferentAmounts_NoCluster_Insufficient(t *testing.T) {
	txs := []banklink.Transaction{
		credit(30, money.FromNaira(50_000)),
		credit(60, money.FromNaira(120_000)),
		credit(90, money.FromNaira(75_000)),
	}
	_, err := PredictNextPayday(txs, predictionNow)
	assert.ErrorIs(t, err, ErrInsufficientPaydayHistory)
}

func TestPredictNextPayday_SameAmountIrregularDates_Insufficient(t *testing.T) {
	// Same amount, but wildly inconsistent spacing — not a payday pattern.
	txs := []banklink.Transaction{
		credit(2, money.FromNaira(100_000)),
		credit(10, money.FromNaira(100_000)),
		credit(45, money.FromNaira(100_000)),
		credit(50, money.FromNaira(100_000)),
	}
	_, err := PredictNextPayday(txs, predictionNow)
	assert.ErrorIs(t, err, ErrInsufficientPaydayHistory)
}

func TestPredictNextPayday_OutOfCadenceRange_Insufficient(t *testing.T) {
	// Perfectly regular, but quarterly (~90 days) — outside the payday
	// cadence range this function is willing to predict against.
	txs := recurringCredits(3, 90, 5, money.FromNaira(500_000))
	_, err := PredictNextPayday(txs, predictionNow)
	assert.ErrorIs(t, err, ErrInsufficientPaydayHistory)
}

func TestPredictNextPayday_IgnoresDebits(t *testing.T) {
	// A perfectly regular DEBIT (e.g. a rent payment) must never be mistaken
	// for income.
	txs := []banklink.Transaction{
		debit(1, money.FromNaira(80_000)),
		debit(31, money.FromNaira(80_000)),
		debit(61, money.FromNaira(80_000)),
		debit(91, money.FromNaira(80_000)),
	}
	_, err := PredictNextPayday(txs, predictionNow)
	assert.ErrorIs(t, err, ErrInsufficientPaydayHistory)
}

func TestPredictNextPayday_IgnoresFutureDatedTransactions(t *testing.T) {
	txs := recurringCredits(4, 30, 1, money.FromNaira(300_000))
	// A future-dated credit must not count, even though it fits the pattern.
	txs = append(txs, banklink.Transaction{
		Date:      predictionNow.AddDate(0, 0, 29),
		Amount:    money.FromNaira(300_000),
		Direction: banklink.Credit,
	})
	pred, err := PredictNextPayday(txs, predictionNow)
	require.NoError(t, err)
	assert.Equal(t, 4, pred.OccurrencesSeen, "the future-dated transaction must be excluded")
}

func TestPredictNextPayday_RecurringPaycheckAmongUnrelatedTransfers(t *testing.T) {
	// A real recurring paycheck plus several one-off, differently-sized
	// transfers — the algorithm must find the paycheck cluster, not average
	// everything together.
	txs := recurringCredits(4, 30, 2, money.FromNaira(250_000))
	txs = append(txs,
		credit(15, money.FromNaira(5_000)),  // a small one-off transfer in
		credit(48, money.FromNaira(12_000)), // another
		debit(20, money.FromNaira(30_000)),  // an unrelated debit
	)
	pred, err := PredictNextPayday(txs, predictionNow)
	require.NoError(t, err)
	assert.Equal(t, 4, pred.OccurrencesSeen)
	assert.Equal(t, money.FromNaira(250_000), pred.AverageAmountKobo)
}

func TestPredictNextPayday_ToleratesSmallAmountJitter(t *testing.T) {
	// Real paychecks aren't always bit-for-bit identical (overtime, a
	// rounded deduction) — small variance within tolerance must still cluster.
	txs := []banklink.Transaction{
		credit(1, money.FromNaira(300_000)),
		credit(31, money.FromNaira(305_000)),
		credit(61, money.FromNaira(298_000)),
		credit(91, money.FromNaira(302_000)),
	}
	pred, err := PredictNextPayday(txs, predictionNow)
	require.NoError(t, err)
	assert.Equal(t, 4, pred.OccurrencesSeen)
}

func TestPredictNextPayday_LargeAmountJitterDoesNotCluster(t *testing.T) {
	// Every amount is more than amountClusterTolerance away from every
	// other — none may be treated as the same recurring payment, so no
	// cluster of size >= 2 can form at all.
	txs := []banklink.Transaction{
		credit(1, money.FromNaira(100_000)),
		credit(31, money.FromNaira(250_000)),
		credit(61, money.FromNaira(400_000)),
	}
	_, err := PredictNextPayday(txs, predictionNow)
	assert.ErrorIs(t, err, ErrInsufficientPaydayHistory)
}
