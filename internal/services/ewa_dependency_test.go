package services

import (
	"testing"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var scoringNow = time.Date(2026, 8, 14, 12, 0, 0, 0, time.UTC)

// draws builds a history of evenly spaced draws ending daysAgoLast before now.
func draws(n int, spacingDays int, daysAgoLast int, amount money.Kobo) []DrawEvent {
	events := make([]DrawEvent, 0, n)
	for i := 0; i < n; i++ {
		at := scoringNow.AddDate(0, 0, -(daysAgoLast + i*spacingDays))
		events = append(events, DrawEvent{At: at, Amount: amount})
	}
	return events
}

func TestScoreDependency_NoHistoryIsHealthy(t *testing.T) {
	got := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		MonthlySalary: money.FromNaira(300_000),
	})

	assert.Equal(t, 0, got.Score)
	assert.Equal(t, models.TierHealthy, got.Tier)
}

func TestScoreDependency_OccasionalUseStaysHealthy(t *testing.T) {
	// Three small draws across three months — the shape of genuine shock absorption.
	got := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		Draws:         draws(3, 30, 5, money.FromNaira(10_000)),
		MonthlySalary: money.FromNaira(300_000),
	})

	assert.Less(t, got.Score, tierElevatedFloor,
		"occasional modest use must not be flagged; score=%d signals=%v", got.Score, got.Signals)
	assert.Equal(t, models.TierHealthy, got.Tier)
}

func TestScoreDependency_HeavyFrequentUseIsFlagged(t *testing.T) {
	// Weekly draws of a large share of salary for three months: ~4/month is above
	// the ~2.5/month population average, and 12 x N40k against N900k of window
	// earnings is >50% utilization.
	got := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		Draws:         draws(12, 7, 1, money.FromNaira(40_000)),
		MonthlySalary: money.FromNaira(300_000),
	})

	assert.GreaterOrEqual(t, got.Score, tierElevatedFloor,
		"weekly large draws must at least reach elevated; score=%d signals=%v", got.Score, got.Signals)
	assert.NotEmpty(t, got.Reasons)
}

// The population average is ~2.5 draws/month. Someone at or near it must not be
// flagged at all — the earlier calibration would have penalised the median user.
func TestScoreDependency_AverageUserIsNotFlaggedOnFrequency(t *testing.T) {
	// ~2.5 draws/month for three months, modest amounts.
	got := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		Draws:         draws(8, 11, 2, money.FromNaira(15_000)),
		MonthlySalary: money.FromNaira(300_000),
	})

	assert.Zero(t, got.Signals["frequency"],
		"a user at the population average must score zero on frequency; signals=%v", got.Signals)
	assert.Equal(t, models.TierHealthy, got.Tier)
}

// Drawing shortly BEFORE payday is ordinary cash-flow smoothing (46% of users do
// it 3-5 days out) and must not be penalised.
func TestScoreDependency_PrePaydayDrawsAreNotPenalised(t *testing.T) {
	payday := scoringNow.AddDate(0, 0, -2)
	// Draws four days before that payday, i.e. six days ago.
	history := []DrawEvent{
		{At: payday.AddDate(0, 0, -4), Amount: money.FromNaira(20_000)},
		{At: payday.AddDate(0, 0, -34), Amount: money.FromNaira(20_000)},
	}

	got := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		Draws:         history,
		MonthlySalary: money.FromNaira(300_000),
		LastPayday:    payday,
	})

	assert.Zero(t, got.Signals["immediacy"],
		"pre-payday draws are normal smoothing and must not score; signals=%v", got.Signals)
}

// A worker whose usage has saturated -- maxed on both frequency and utilization
// but no longer rising -- has completed the trajectory, not avoided it. Scoring
// escalation purely as growth would cap them below the dependent tier forever.
func TestScoreDependency_SaturatedUserReachesDependent(t *testing.T) {
	// Uniform daily draws: no month-over-month growth at all.
	got := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		Draws:         draws(60, 1, 0, money.FromNaira(50_000)),
		MonthlySalary: money.FromNaira(300_000),
	})

	assert.Equal(t, maxEscalationScore, got.Signals["escalation"],
		"saturation must count as completed escalation; signals=%v", got.Signals)
	assert.Equal(t, models.TierDependent, got.Tier,
		"a saturated user is the clearest dependency case; score=%d", got.Score)
}

func TestScoreDependency_EscalationIsDetected(t *testing.T) {
	// One draw in the 30–60d window, four in the last 30 days: usage doubling,
	// which is the published trajectory of habituation.
	history := []DrawEvent{
		{At: scoringNow.AddDate(0, 0, -45), Amount: money.FromNaira(20_000)},
		{At: scoringNow.AddDate(0, 0, -25), Amount: money.FromNaira(20_000)},
		{At: scoringNow.AddDate(0, 0, -18), Amount: money.FromNaira(20_000)},
		{At: scoringNow.AddDate(0, 0, -10), Amount: money.FromNaira(20_000)},
		{At: scoringNow.AddDate(0, 0, -3), Amount: money.FromNaira(20_000)},
	}

	got := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		Draws:         history,
		MonthlySalary: money.FromNaira(300_000),
	})

	assert.Positive(t, got.Signals["escalation"],
		"rising month-over-month usage must register; signals=%v", got.Signals)
}

func TestScoreDependency_ImmediacyAfterPayday(t *testing.T) {
	payday := scoringNow.AddDate(0, 0, -14)
	history := []DrawEvent{
		// Drawn the day after being paid — the cheque did not last.
		{At: payday.Add(24 * time.Hour), Amount: money.FromNaira(30_000)},
		{At: payday.Add(48 * time.Hour), Amount: money.FromNaira(30_000)},
	}

	withPayday := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		Draws:         history,
		MonthlySalary: money.FromNaira(300_000),
		LastPayday:    payday,
	})
	withoutPayday := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		Draws:         history,
		MonthlySalary: money.FromNaira(300_000),
	})

	assert.Positive(t, withPayday.Signals["immediacy"])
	assert.Greater(t, withPayday.Score, withoutPayday.Score,
		"drawing straight after payday must raise the score")
}

func TestScoreDependency_ScoreIsBounded(t *testing.T) {
	payday := scoringNow.AddDate(0, 0, -2)
	history := draws(60, 1, 0, money.FromNaira(100_000))
	history = append(history,
		DrawEvent{At: payday.Add(time.Hour), Amount: money.FromNaira(100_000)},
		DrawEvent{At: payday.Add(2 * time.Hour), Amount: money.FromNaira(100_000)},
	)

	got := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		Draws:         history,
		MonthlySalary: money.FromNaira(300_000),
		LastPayday:    payday,
	})

	assert.LessOrEqual(t, got.Score, 100)
	assert.Equal(t, models.TierDependent, got.Tier)
}

func TestScoreDependency_IgnoresDrawsOutsideWindow(t *testing.T) {
	old := []DrawEvent{
		{At: scoringNow.AddDate(0, 0, -200), Amount: money.FromNaira(50_000)},
		{At: scoringNow.AddDate(0, 0, -150), Amount: money.FromNaira(50_000)},
	}

	got := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		Draws:         old,
		MonthlySalary: money.FromNaira(300_000),
	})

	assert.Equal(t, 0, got.Score, "history older than the window must not count")
}

func TestTierFor_Boundaries(t *testing.T) {
	cases := []struct {
		score int
		want  models.DependencyTier
	}{
		{0, models.TierHealthy},
		{tierElevatedFloor - 1, models.TierHealthy},
		{tierElevatedFloor, models.TierElevated},
		{tierStrainedFloor - 1, models.TierElevated},
		{tierStrainedFloor, models.TierStrained},
		{tierDependentFloor - 1, models.TierStrained},
		{tierDependentFloor, models.TierDependent},
		{100, models.TierDependent},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, tierFor(c.score), "score %d", c.score)
	}
}

// The central product-safety property: escalating guardrails must never cut a
// worker off completely. A hard zero pushes people to payday lenders, which is
// the outcome the whole feature exists to avoid.
func TestTierCap_NeverDropsToZero(t *testing.T) {
	policy := models.DefaultEWAPolicy("ORG-1")
	policyCap := money.FromNaira(150_000)

	for _, tier := range []models.DependencyTier{
		models.TierHealthy, models.TierElevated, models.TierStrained, models.TierDependent,
	} {
		cap := TierCap(tier, policyCap, policy)
		assert.True(t, cap.IsPositive(), "tier %s must retain access, got %s", tier, cap)
		assert.GreaterOrEqual(t, int64(cap), int64(policy.EmergencyFloorKobo),
			"tier %s must keep at least the emergency floor", tier)
	}
}

func TestTierCap_TightensAsDependencyDeepens(t *testing.T) {
	policy := models.DefaultEWAPolicy("ORG-1")
	policyCap := money.FromNaira(150_000)

	healthy := TierCap(models.TierHealthy, policyCap, policy)
	elevated := TierCap(models.TierElevated, policyCap, policy)
	strained := TierCap(models.TierStrained, policyCap, policy)
	dependent := TierCap(models.TierDependent, policyCap, policy)

	assert.Equal(t, policyCap, healthy)
	assert.Equal(t, policyCap, elevated, "elevated differs by nudge, not by cap")
	assert.Less(t, int64(strained), int64(elevated))
	assert.Less(t, int64(dependent), int64(strained))
}

// A cap already below the emergency floor must not be inflated up to it — the
// floor is a protection against over-restriction, not an entitlement to more
// than the worker has actually earned.
func TestTierCap_DoesNotExceedEarnedCap(t *testing.T) {
	policy := models.DefaultEWAPolicy("ORG-1")
	tiny := money.FromNaira(2_000) // below the ₦5,000 emergency floor

	for _, tier := range []models.DependencyTier{models.TierStrained, models.TierDependent} {
		cap := TierCap(tier, tiny, policy)
		assert.LessOrEqual(t, int64(cap), int64(tiny),
			"tier %s must never allow more than the earned cap", tier)
	}
}

func TestTierCoolingOff_DoublesUnderStrain(t *testing.T) {
	policy := models.DefaultEWAPolicy("ORG-1")
	base := time.Duration(policy.CoolingOffHours) * time.Hour

	assert.Equal(t, base, TierCoolingOff(models.TierHealthy, policy))
	assert.Equal(t, base, TierCoolingOff(models.TierElevated, policy))
	assert.Equal(t, 2*base, TierCoolingOff(models.TierStrained, policy))
	assert.Equal(t, 2*base, TierCoolingOff(models.TierDependent, policy))
}

func TestScaleScore(t *testing.T) {
	assert.Equal(t, 0, scaleScore(0.5, 1, 4, 30), "below the low mark scores zero")
	assert.Equal(t, 30, scaleScore(4, 1, 4, 30), "at the high mark scores max")
	assert.Equal(t, 30, scaleScore(9, 1, 4, 30), "above the high mark clamps")
	assert.Equal(t, 10, scaleScore(2, 1, 4, 30), "linear in between")
}

func TestTierNudge_HealthyHasNone(t *testing.T) {
	assert.Empty(t, TierNudge(models.TierHealthy))
}

func TestTierNudge_ElevatedAndAboveAllHaveDistinctMessages(t *testing.T) {
	tiers := []models.DependencyTier{models.TierElevated, models.TierStrained, models.TierDependent}
	seen := map[string]bool{}
	for _, tier := range tiers {
		msg := TierNudge(tier)
		assert.NotEmpty(t, msg, "tier %s must have a nudge message", tier)
		assert.False(t, seen[msg], "tier %s reuses another tier's message verbatim", tier)
		seen[msg] = true
	}
}

// ScoreDependency must wire Nudge from the final Tier — reusing the known
// saturated-user scenario from TestScoreDependency_SaturatedUserReachesDependent
// rather than hand-constructing a new draw history, since getting the scoring
// math to land on a specific tier by hand is exactly the kind of arithmetic
// this package's own tests exist to double-check, not assume.
func TestScoreDependency_NudgeMatchesFinalTier(t *testing.T) {
	got := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		Draws:         draws(60, 1, 0, money.FromNaira(50_000)),
		MonthlySalary: money.FromNaira(300_000),
	})
	require.Equal(t, models.TierDependent, got.Tier, "test setup must actually land on Dependent")
	assert.Equal(t, TierNudge(models.TierDependent), got.Nudge)
	assert.NotEmpty(t, got.Nudge)
}

func TestScoreDependency_HealthyNudgeIsEmpty(t *testing.T) {
	got := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		MonthlySalary: money.FromNaira(300_000),
	})
	assert.Equal(t, models.TierHealthy, got.Tier)
	assert.Empty(t, got.Nudge)
}

func TestScoreDependency_SignalsAlwaysPopulated(t *testing.T) {
	got := ScoreDependency(DependencyInput{
		Now:           scoringNow,
		Draws:         draws(2, 20, 5, money.FromNaira(15_000)),
		MonthlySalary: money.FromNaira(300_000),
	})

	for _, key := range []string{"frequency", "utilization", "escalation", "immediacy"} {
		_, ok := got.Signals[key]
		require.True(t, ok, "signal %q must always be reported for explainability", key)
	}
}
