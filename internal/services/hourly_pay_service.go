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

	// EntryIDs are every TimeEntry this breakdown paid for — regular AND
	// overtime, from this period and any earlier one swept in. The caller
	// must pass these to models.MarkTimeEntriesPaid in the same transaction
	// that creates the payroll item, or they will be paid again next run.
	EntryIDs []string
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
// Entries are processed oldest-first (see UnpaidApprovedEntriesThrough) so
// an entry that straddles the threshold is split proportionally between
// regular and overtime.
//
// Entries are never scoped to just this period: an approved entry from an
// earlier period that missed its own period's run (a late approval) is
// swept into this one instead of being silently dropped — see migration
// 000028. Because a swept entry's ISO week may have already been partly
// paid by that earlier run, this seeds each week's running total from
// PaidMinutesInISOWeek before attributing any of THIS run's minutes, so a
// late entry in an already-overtime week is still correctly rated as
// overtime rather than underpaid as regular time.
func ComputeHourlyGross(
	tx *gorm.DB, orgID, employeeID, period string, asOf time.Time, hourlyRateKobo money.Kobo,
) (HourlyPayBreakdown, error) {
	var result HourlyPayBreakdown

	policy, err := payrollPolicyTx(tx, orgID)
	if err != nil {
		return result, fmt.Errorf("payroll policy lookup failed: %w", err)
	}

	entries, err := models.UnpaidApprovedEntriesThrough(tx, orgID, employeeID, period, asOf)
	if err != nil {
		return result, fmt.Errorf("unpaid approved entries lookup failed: %w", err)
	}

	weekMinutes := map[isoWeekKey]int64{}
	seededWeeks := map[isoWeekKey]bool{}
	var baseAmounts, premiumAmounts []money.Kobo
	entryIDs := make([]string, 0, len(entries))

	for _, entry := range entries {
		year, week := entry.WorkDate.ISOWeek()
		key := isoWeekKey{year, week}
		if !seededWeeks[key] {
			paid, err := models.PaidMinutesInISOWeek(tx, orgID, employeeID, year, week)
			if err != nil {
				return result, fmt.Errorf("paid-minutes lookup for ISO week %d-W%02d failed: %w", year, week, err)
			}
			weekMinutes[key] = paid
			seededWeeks[key] = true
		}
		entryIDs = append(entryIDs, entry.ID)
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
	result.EntryIDs = entryIDs
	return result, nil
}
