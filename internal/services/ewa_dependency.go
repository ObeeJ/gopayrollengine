package services

import (
	"sort"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"
)

// Dependency scoring.
//
// Calibrated against the 2026-08 evidence review (docs/EWA_ROADMAP.md §1). Two
// corrections from the original implementation are worth stating, because both
// were wrong in ways that would have shaped the product badly:
//
// 1. USAGE DOES NOT SIMPLY ESCALATE FOR EVERYONE. The earlier thresholds assumed
//    a "2/month → 4/month in year one" doubling. The strongest independent
//    dataset (CFPB, employer-integrated providers) shows 22.9 → 29.8
//    transactions/user/year — roughly 1.9 → 2.5 per month, about +30%. The
//    distribution is U-shaped: over half draw once a month or less, while about
//    a quarter draw more than twice a month. So the population is light users,
//    ordinary repeat users, and a heavy tail — not everyone sliding into
//    dependency. Frequency alone must therefore NOT flag anyone near the
//    average, and trajectory matters more than volume. Escalation is now the
//    highest-weighted signal for exactly that reason.
//
// 2. DRAWING BEFORE PAYDAY IS NORMAL, NOT A WARNING SIGN. Federal Reserve data
//    shows 46% of users request funds 3–5 days before payday and a further 27%
//    1–2 days before. That is ordinary end-of-cycle cash-flow smoothing and is
//    deliberately not penalised here. The abnormal pattern is the opposite one:
//    drawing again in the days immediately AFTER being paid, which means the
//    full-paycheck reset has disappeared. That is what `immediacy` measures, and
//    it is weighted lower than before because its base rate is smaller than the
//    "75% re-draw immediately" vendor claim suggested.
//
// Four signals, scored independently so one unusual month cannot flag someone:
//
//	frequency   — how often they draw, relative to the population average
//	utilization — how much of what they earn is drawn
//	escalation  — whether the pattern is deepening (highest weight)
//	immediacy   — whether they draw again straight after being paid
//
// WHY ACCESS NEVER DROPS TO ZERO — corrected justification.
// The original comment here claimed a hard block pushes workers to payday
// lenders. The evidence does not support that: asked what they did when they
// could not access EWA, users reported going without something they needed
// (36%), borrowing from friends or family (31%), and credit cards (26%);
// regulated small loans did not emerge as the obvious substitute. Nigerian
// survey data points the same way — 47% cut expenses when income fell.
//
// The floor therefore exists for a narrower and more defensible reason: the
// most common consequence of denial is a worker going without something they
// need. That is real harm, and it is harm we would cause. It is NOT a licence
// to keep caps high out of fear of substitution — which is precisely the error
// the earlier reasoning invited.

// Scoring window and sub-score ceilings.
//
// Weights: utilization and escalation dominate. Utilization carries the most
// because it measures the harm directly — the share of payday already spent
// before it arrives — whereas frequency is only a proxy for it. Raw frequency is
// deliberately weakest: the average user draws ~2.5 times a month and must not
// be flagged merely for being average.
const (
	dependencyWindow = 90 * 24 * time.Hour
	recentWindow     = 30 * 24 * time.Hour

	maxFrequencyScore   = 20
	maxUtilizationScore = 35
	maxEscalationScore  = 30
	maxImmediacyScore   = 15

	// A draw within this long after payday means the previous cheque did not last.
	immediacyThreshold = 72 * time.Hour

	// Frequency scale. The population average is ~2.5 draws/month, so scoring
	// starts above it: an average user contributes zero here.
	freqScoreFloor = 3.0
	freqScoreCeil  = 8.0

	// Utilization scale. Nigerian employee cooperatives commonly cap salary
	// advances at 30% of basic pay, which makes that share the culturally
	// legible ceiling rather than an arbitrary one.
	utilScoreFloor = 0.20
	utilScoreCeil  = 0.50

	// Escalation scale. The observed year-over-year aggregate rise is ~30%, so
	// growth beyond that is the meaningful signal.
	escalationScoreFloor = 0.30
	escalationScoreCeil  = 1.00
)

// Tier boundaries.
//
// The elevated floor sits just below maxUtilizationScore on purpose: a worker
// drawing half or more of their earnings before payday earns a nudge on that
// basis alone, without needing a second signal to corroborate it. Elevated
// costs them nothing — full access, plus the information — so triggering it
// early is cheap, and triggering it late is the failure that matters.
const (
	tierElevatedFloor  = 35
	tierStrainedFloor  = 60
	tierDependentFloor = 80
)

// DrawEvent — one historical advance, reduced to what scoring needs.
type DrawEvent struct {
	At     time.Time
	Amount money.Kobo
}

// DependencyInput — everything the score depends on. No DB access, no clock
// reads: both are injected so the result is reproducible and testable.
type DependencyInput struct {
	Now time.Time
	// Draws within the scoring window. Order does not matter.
	Draws []DrawEvent
	// MonthlySalary is the denominator for utilization.
	MonthlySalary money.Kobo
	// LastPayday drives the immediacy signal. Zero disables that component.
	LastPayday time.Time
}

// DependencyAssessment — the score, its tier, and the reasons behind it.
type DependencyAssessment struct {
	Score   int                   `json:"score"`
	Tier    models.DependencyTier `json:"tier"`
	Signals map[string]int        `json:"signals"`
	// Reasons are worker-facing explanations. They must read as observations,
	// not accusations — this text is shown to the person it describes.
	Reasons []string `json:"reasons,omitempty"`
}

// ScoreDependency computes the dependency assessment. Pure function.
func ScoreDependency(in DependencyInput) DependencyAssessment {
	windowStart := in.Now.Add(-dependencyWindow)
	recentStart := in.Now.Add(-recentWindow)
	priorStart := in.Now.Add(-2 * recentWindow)

	inWindow := make([]DrawEvent, 0, len(in.Draws))
	for _, d := range in.Draws {
		if d.At.After(windowStart) && !d.At.After(in.Now) {
			inWindow = append(inWindow, d)
		}
	}
	sort.Slice(inWindow, func(i, j int) bool { return inWindow[i].At.Before(inWindow[j].At) })

	assessment := DependencyAssessment{
		Tier:    models.TierHealthy,
		Signals: map[string]int{},
	}
	if len(inWindow) == 0 {
		assessment.Signals["frequency"] = 0
		assessment.Signals["utilization"] = 0
		assessment.Signals["escalation"] = 0
		assessment.Signals["immediacy"] = 0
		return assessment
	}

	// --- Frequency ------------------------------------------------------------
	// Draws per 30 days across the window.
	drawsPerMonth := float64(len(inWindow)) / (float64(dependencyWindow) / float64(recentWindow))
	freq := scaleScore(drawsPerMonth, freqScoreFloor, freqScoreCeil, maxFrequencyScore)
	assessment.Signals["frequency"] = freq
	if drawsPerMonth >= freqScoreFloor {
		assessment.Reasons = append(assessment.Reasons,
			"You've been drawing early pay more often than most people do.")
	}

	// --- Utilization ----------------------------------------------------------
	// Share of earnings drawn early over the window. Salary is monthly, the
	// window is three months, so the denominator is 3× salary.
	var utilization float64
	if in.MonthlySalary.IsPositive() {
		var drawn money.Kobo
		for _, d := range inWindow {
			if sum, err := drawn.Add(d.Amount); err == nil {
				drawn = sum
			}
		}
		windowEarnings := float64(in.MonthlySalary) * (float64(dependencyWindow) / float64(30*24*time.Hour))
		if windowEarnings > 0 {
			utilization = float64(drawn) / windowEarnings
		}
	}
	util := scaleScore(utilization, utilScoreFloor, utilScoreCeil, maxUtilizationScore)
	assessment.Signals["utilization"] = util
	if utilization >= 0.30 {
		assessment.Reasons = append(assessment.Reasons,
			"Close to a third of your recent pay has been drawn before payday.")
	}

	// --- Escalation -----------------------------------------------------------
	// Recent 30 days against the 30 before that. Rising usage is the signal that
	// separates a rough patch from a deepening habit.
	var recent, prior int
	for _, d := range inWindow {
		switch {
		case d.At.After(recentStart):
			recent++
		case d.At.After(priorStart):
			prior++
		}
	}
	escalation := 0
	if prior > 0 && recent > prior {
		growth := float64(recent-prior) / float64(prior)
		escalation = scaleScore(growth, escalationScoreFloor, escalationScoreCeil, maxEscalationScore)
	} else if prior == 0 && recent >= 3 {
		// No history then straight to frequent use is its own kind of signal.
		escalation = maxEscalationScore / 2
	}

	// Saturation. Escalation measures trajectory, but a worker who has already
	// reached the ceiling on both frequency and utilization has *completed* that
	// trajectory — their usage stopped rising only because there is nowhere left
	// to rise to. Without this, a stable extreme user scores at most
	// frequency+utilization+immediacy and could never be classified dependent,
	// which is precisely backwards: they are the clearest case there is.
	saturated := freq >= maxFrequencyScore && util >= maxUtilizationScore
	if saturated && escalation < maxEscalationScore {
		escalation = maxEscalationScore
	}

	assessment.Signals["escalation"] = escalation
	switch {
	case saturated:
		assessment.Reasons = append(assessment.Reasons,
			"Almost all of your pay is being drawn before payday, every period.")
	case escalation >= maxEscalationScore/2:
		assessment.Reasons = append(assessment.Reasons,
			"Your use of early pay has been increasing month over month.")
	}

	// --- Immediacy ------------------------------------------------------------
	// Draws taken within 72h AFTER the last payday. If pay runs out immediately,
	// the full-paycheck reset has disappeared and the advance is no longer
	// bridging a gap — it has become part of the budget.
	// Note the asymmetry: draws in the days BEFORE payday are ordinary cash-flow
	// smoothing and score nothing. Only draws in the days AFTER being paid count.
	immediacy := 0
	if !in.LastPayday.IsZero() {
		postPayday := 0
		for _, d := range inWindow {
			if d.At.After(in.LastPayday) && d.At.Sub(in.LastPayday) <= immediacyThreshold {
				postPayday++
			}
		}
		if postPayday > 0 {
			immediacy = scaleScore(float64(postPayday), 1.0, 3.0, maxImmediacyScore)
			assessment.Reasons = append(assessment.Reasons,
				"You've needed early pay within a few days of being paid.")
		}
	}
	assessment.Signals["immediacy"] = immediacy

	assessment.Score = freq + util + escalation + immediacy
	if assessment.Score > 100 {
		assessment.Score = 100
	}
	assessment.Tier = tierFor(assessment.Score)
	return assessment
}

// tierFor maps a score onto its tier.
func tierFor(score int) models.DependencyTier {
	switch {
	case score >= tierDependentFloor:
		return models.TierDependent
	case score >= tierStrainedFloor:
		return models.TierStrained
	case score >= tierElevatedFloor:
		return models.TierElevated
	default:
		return models.TierHealthy
	}
}

// scaleScore maps value linearly onto [0, max] between the low and high marks.
// Below low scores zero; at or above high scores max.
func scaleScore(value, low, high float64, max int) int {
	if value <= low {
		return 0
	}
	if value >= high {
		return max
	}
	return int(float64(max) * (value - low) / (high - low))
}

// TierCap applies the tier's response to the policy cap.
//
// This is the whole harm-reduction argument in one function: access narrows as
// dependency deepens, but never to zero. The emergency floor is what stops a
// guardrail from becoming the reason someone takes a 300% APR loan instead.
func TierCap(tier models.DependencyTier, policyCap money.Kobo, policy models.EWAPolicy) money.Kobo {
	switch tier {
	case models.TierDependent:
		// Emergency access only, and a human should be looking at this account.
		if policy.EmergencyFloorKobo < policyCap {
			return policy.EmergencyFloorKobo
		}
		return policyCap
	case models.TierStrained:
		// Half the normal cap, never below the emergency floor.
		halved, err := policyCap.Percent(50, 100)
		if err != nil {
			return policy.EmergencyFloorKobo
		}
		if halved < policy.EmergencyFloorKobo {
			if policy.EmergencyFloorKobo < policyCap {
				return policy.EmergencyFloorKobo
			}
			return policyCap
		}
		return halved
	default:
		// healthy and elevated keep the full cap; elevated differs by the nudge
		// shown to the worker, not by how much they may draw.
		return policyCap
	}
}

// TierCoolingOff returns the wait between draws for a tier. Friction is applied
// where it changes behaviour — between draws — rather than at the moment of need.
func TierCoolingOff(tier models.DependencyTier, policy models.EWAPolicy) time.Duration {
	base := time.Duration(policy.CoolingOffHours) * time.Hour
	switch tier {
	case models.TierStrained, models.TierDependent:
		return base * 2
	default:
		return base
	}
}
