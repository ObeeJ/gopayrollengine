package models

import "time"

// PayrollPolicy — per-org configuration for shift-differential and overtime
// pay rules, applied to hourly/gig gross at payroll time (see
// services.ComputeHourlyGross). Distinct from EWAPolicy, which governs draw
// guardrails, not wage calculation.
//
// Multipliers are basis points (10000 = 1.0x) to match this codebase's
// existing percent-based money math, and are floored at 10000 by the DB CHECK
// constraints in migration 000027: a "differential" below the base rate isn't
// a differential, and this table must not be capable of silently cutting pay.
type PayrollPolicy struct {
	OrganizationID string `gorm:"primaryKey" json:"organization_id"`

	// OvertimeThresholdMinutesPerWeek is the calendar-week (Mon-Sun) minute
	// count beyond which further approved minutes are overtime. 2400 = 40h.
	OvertimeThresholdMinutesPerWeek int `gorm:"column:overtime_threshold_minutes_per_week;default:2400" json:"overtime_threshold_minutes_per_week"`
	// OvertimeMultiplierBps is applied only to the portion of a week's
	// minutes past the threshold. 15000 = time-and-a-half.
	OvertimeMultiplierBps int `gorm:"column:overtime_multiplier_bps;default:15000" json:"overtime_multiplier_bps"`

	NightShiftMultiplierBps   int `gorm:"column:night_shift_multiplier_bps;default:10000" json:"night_shift_multiplier_bps"`
	WeekendShiftMultiplierBps int `gorm:"column:weekend_shift_multiplier_bps;default:10000" json:"weekend_shift_multiplier_bps"`
	HolidayShiftMultiplierBps int `gorm:"column:holiday_shift_multiplier_bps;default:10000" json:"holiday_shift_multiplier_bps"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (PayrollPolicy) TableName() string { return "payroll_policies" }

// DefaultPayrollPolicy — the values an org gets before anyone tunes anything:
// standard 40h/week overtime at time-and-a-half, no shift differentials.
func DefaultPayrollPolicy(orgID string) PayrollPolicy {
	return PayrollPolicy{
		OrganizationID:                  orgID,
		OvertimeThresholdMinutesPerWeek: 2400,
		OvertimeMultiplierBps:           15000,
		NightShiftMultiplierBps:         10000,
		WeekendShiftMultiplierBps:       10000,
		HolidayShiftMultiplierBps:       10000,
	}
}

// MultiplierBps returns the differential multiplier for a shift type. Regular
// (and any unrecognized value) is always 10000 — no premium.
func (p PayrollPolicy) MultiplierBps(shiftType TimeEntryShiftType) int {
	switch shiftType {
	case ShiftNight:
		return p.NightShiftMultiplierBps
	case ShiftWeekend:
		return p.WeekendShiftMultiplierBps
	case ShiftHoliday:
		return p.HolidayShiftMultiplierBps
	default:
		return 10000
	}
}
