package services

import (
	"errors"
	"fmt"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
)

// HourlyPayBreakdown is the result of ComputeHourlyGross: gross pay for an
// hourly employee's approved entries in a period, plus the figures an
// employer needs to explain the number on a payslip.
type HourlyPayBreakdown struct {
	RegularMinutes  int64
	OvertimeMinutes int64

	// BaseKobo is pay for every approved minute (regular AND overtime) at
	// whatever shift differential each entry claims — overtime status never
	// reduces the shift-differential rate an hour is paid at.
	BaseKobo money.Kobo
	// OvertimePremiumKobo is the strictly additional amount paid on top of
	// BaseKobo for the overtime portion only: the "extra half" in
	// time-and-a-half, layered on top of whatever shift differential that
	// portion already earned. A night-shift overtime hour earns both.
	OvertimePremiumKobo money.Kobo
	// GrossKobo is BaseKobo + OvertimePremiumKobo.
	GrossKobo money.Kobo
}

// payrollPolicyTx loads the org's PayrollPolicy, falling back to the default
// when none has ever been saved — the same fallback pattern as EWAService's
// policyTx, so an org's first-ever configuration and every update after it
// go through the identical write path.
func payrollPolicyTx(tx *gorm.DB, orgID string) (*models.PayrollPolicy, error) {
	var policy models.PayrollPolicy
	err := tx.First(&policy, "organization_id = ?", orgID).Error
	if err == nil {
		return &policy, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	def := models.DefaultPayrollPolicy(orgID)
	return &def, nil
}

// isoWeekKey identifies a calendar week (Mon-Sun) regardless of year
// boundary — time.Time.ISOWeek() already handles the case where the first or
// last days of a year belong to a week numbered in the adjacent year.
type isoWeekKey struct {
	year, week int
}

// ComputeHourlyGross computes an hourly/gig employee's gross pay for `period`
// from their approved TimeEntry rows, applying:
//
//  1. Shift differentials (models.PayrollPolicy.MultiplierBps) to every
//     approved minute, regular or overtime — a night shift is paid at the
//     night rate whether or not it also happens to be overtime.
//  2. A weekly overtime premium: once an employee's approved minutes in a
//     calendar week (Mon-Sun, via ISOWeek) cross
//     OvertimeThresholdMinutesPerWeek, further minutes in that week earn an
//     ADDITIONAL premium — never a replacement for the shift differential —
//     on just the overtime portion.
//
// Entries are processed oldest-first (see ApprovedEntriesForPeriod) so an
// entry that straddles the threshold is split proportionally between regular
// and overtime.
//
// Weekly attribution is scoped to this period's entries only: a week
// spanning a period boundary is attributed independently in each period's
// run, the same boundary tradeoff SumApprovedMinutes already accepts for
// monthly accrual. See docs/EWA_ROADMAP.md Phase 5's next item for sweeping
// late-approved entries; this function does not attempt to fix that here.
func ComputeHourlyGross(
	tx *gorm.DB, orgID, employeeID, period string, asOf time.Time, hourlyRateKobo money.Kobo,
) (HourlyPayBreakdown, error) {
	var result HourlyPayBreakdown

	policy, err := payrollPolicyTx(tx, orgID)
	if err != nil {
		return result, fmt.Errorf("payroll policy lookup failed: %w", err)
	}

	entries, err := models.ApprovedEntriesForPeriod(tx, orgID, employeeID, period, asOf)
	if err != nil {
		return result, fmt.Errorf("approved entries lookup failed: %w", err)
	}

	weekMinutes := map[isoWeekKey]int64{}
	var baseAmounts, premiumAmounts []money.Kobo

	for _, entry := range entries {
		year, week := entry.WorkDate.ISOWeek()
		key := isoWeekKey{year, week}
		minutesWorked := int64(entry.MinutesWorked)

		before := weekMinutes[key]
		threshold := int64(policy.OvertimeThresholdMinutesPerWeek)
		regularPortion := minutesWorked
		if before >= threshold {
			regularPortion = 0
		} else if before+minutesWorked > threshold {
			regularPortion = threshold - before
		}
		overtimePortion := minutesWorked - regularPortion
		weekMinutes[key] = before + minutesWorked

		result.RegularMinutes += regularPortion
		result.OvertimeMinutes += overtimePortion

		multiplierBps := int64(policy.MultiplierBps(entry.ShiftType))
		// Each Percent call multiplies by a single factor, so MulInt's
		// overflow check actually bounds the value being checked — chaining
		// keeps that true even for a pathologically large admin-configured
		// bps, where multiplying minutes × bps × rate in one raw expression
		// before any check could wrap silently.
		shiftRate, err := hourlyRateKobo.Percent(multiplierBps, 10000)
		if err != nil {
			return result, fmt.Errorf("shift rate computation for entry %s failed: %w", entry.ID, err)
		}
		// Shift-adjusted rate applies to every minute this entry claims,
		// regular or overtime.
		entryBase, err := shiftRate.Percent(minutesWorked, 60)
		if err != nil {
			return result, fmt.Errorf("base pay computation for entry %s failed: %w", entry.ID, err)
		}
		baseAmounts = append(baseAmounts, entryBase)

		if overtimePortion > 0 {
			extraBps := int64(policy.OvertimeMultiplierBps) - 10000
			if extraBps > 0 {
				extraRate, err := shiftRate.Percent(extraBps, 10000)
				if err != nil {
					return result, fmt.Errorf("overtime premium rate computation for entry %s failed: %w", entry.ID, err)
				}
				premium, err := extraRate.Percent(overtimePortion, 60)
				if err != nil {
					return result, fmt.Errorf("overtime premium computation for entry %s failed: %w", entry.ID, err)
				}
				premiumAmounts = append(premiumAmounts, premium)
			}
		}
	}

	base, err := money.Sum(baseAmounts)
	if err != nil {
		return result, fmt.Errorf("base pay total overflow: %w", err)
	}
	premium, err := money.Sum(premiumAmounts)
	if err != nil {
		return result, fmt.Errorf("overtime premium total overflow: %w", err)
	}
	gross, err := base.Add(premium)
	if err != nil {
		return result, fmt.Errorf("gross pay total overflow: %w", err)
	}

	result.BaseKobo = base
	result.OvertimePremiumKobo = premium
	result.GrossKobo = gross
	return result, nil
}
