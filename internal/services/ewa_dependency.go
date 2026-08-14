package services

import (
	"sort"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"
)

// Dependency scoring.
//
// The product risk with earned wage access is not fraud, it is habituation. The
// published evidence is consistent on the shape of it: CFPB's paycheck-advance
// data spotlight found the average user takes ~27 advances a year with roughly
// half drawing at least monthly, and market research repeatedly finds usage
// roughly doubling over a user's first year and a large majority of users
// re-drawing immediately after being paid. A worker in that pattern is not
// bridging a shock — their pay has been permanently pulled forward, and every
// period now starts short.
//
// So the guardrail cannot be a single cap. It has to notice the *pattern* and
// respond before the pattern hardens. Four signals, each scored independently
// so a single unusual month cannot by itself flag someone:
//
//	frequency   — how often they draw
//	utilization — how much of what they earn is drawn
//	escalation  — whether the habit is deepening
//	immediacy   — whether they draw straight after payday
//
// Deliberate design choice: a high score NEVER produces a hard block. Cutting
// someone off does not remove the need for money; it moves it to a payday lender
// at a multiple of the cost. Every tier keeps an emergency floor. The escalating
// response is friction, reduced caps, and a route to help — not a locked door.

// Scoring window and sub-score ceilings.
const (
	dependencyWindow = 90 * 24 * time.Hour
	recentWindow     = 30 * 24 * time.Hour

	maxFrequencyScore   = 30
	maxUtilizationScore = 30
	maxEscalationScore  = 20
	maxImmediacyScore   = 20

	// A draw within this long after payday means the previous cheque did not last.
	immediacyThreshold = 72 * time.Hour
)

// Tier boundaries.
const (
	tierElevatedFloor  = 40
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
	freq := scaleScore(drawsPerMonth, 1.0, 4.0, maxFrequencyScore)
	assessment.Signals["frequency"] = freq
	if drawsPerMonth >= 3 {
		assessment.Reasons = append(assessment.Reasons,
			"You've been drawing early pay about every week or more often.")
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
	util := scaleScore(utilization, 0.10, 0.50, maxUtilizationScore)
	assessment.Signals["utilization"] = util
	if utilization >= 0.35 {
		assessment.Reasons = append(assessment.Reasons,
			"More than a third of your recent pay has been drawn before payday.")
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
		escalation = scaleScore(growth, 0.25, 1.0, maxEscalationScore)
	} else if prior == 0 && recent >= 3 {
		// No history then straight to frequent use is its own kind of signal.
		escalation = maxEscalationScore / 2
	}
	assessment.Signals["escalation"] = escalation
	if escalation >= maxEscalationScore/2 {
		assessment.Reasons = append(assessment.Reasons,
			"Your use of early pay has been increasing month over month.")
	}

	// --- Immediacy ------------------------------------------------------------
	// Draws taken within 72h of the last payday. If pay runs out immediately,
	// the advance is not bridging a gap — it has become part of the budget.
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
