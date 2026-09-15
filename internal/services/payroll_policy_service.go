package services

import (
	"context"
	"encoding/json"

	"go-payroll-engine/internal/models"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PayrollPolicyUpdate carries only the fields an admin is changing — nil
// means leave as-is. Applied to whatever the org's current policy is (the
// default, if none has ever been saved), mirroring EWAService's PolicyUpdate.
type PayrollPolicyUpdate struct {
	OvertimeThresholdMinutesPerWeek *int
	OvertimeMultiplierBps           *int
	NightShiftMultiplierBps         *int
	WeekendShiftMultiplierBps       *int
	HolidayShiftMultiplierBps       *int
}

// GetPayrollPolicy returns the org's PayrollPolicy — the default, unmodified,
// if the org has never saved one.
func (s *PayrollService) GetPayrollPolicy(ctx context.Context, orgID string) (*models.PayrollPolicy, error) {
	var policy *models.PayrollPolicy
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		p, err := payrollPolicyTx(tx, orgID)
		policy = p
		return err
	})
	return policy, err
}

// UpdatePayrollPolicy validates and applies upd to the org's policy, audits
// the before/after, and returns the result. ComputeHourlyGross reads the
// saved row on every payroll run, so this takes effect on the very next run
// — there is no cache to invalidate.
func (s *PayrollService) UpdatePayrollPolicy(ctx context.Context, orgID string, upd PayrollPolicyUpdate, actorIP string) (*models.PayrollPolicy, error) {
	var result *models.PayrollPolicy
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		current, err := payrollPolicyTx(tx, orgID)
		if err != nil {
			return err
		}
		before, err := json.Marshal(current)
		if err != nil {
			return err
		}

		if upd.OvertimeThresholdMinutesPerWeek != nil {
			if *upd.OvertimeThresholdMinutesPerWeek <= 0 {
				return invalidPolicyValue("overtime_threshold_minutes_per_week", "must be positive")
			}
			current.OvertimeThresholdMinutesPerWeek = *upd.OvertimeThresholdMinutesPerWeek
		}
		if upd.OvertimeMultiplierBps != nil {
			if *upd.OvertimeMultiplierBps < 10000 {
				return invalidPolicyValue("overtime_multiplier_bps", "must be at least 10000 (1.0x) — cannot pay less than base rate")
			}
			current.OvertimeMultiplierBps = *upd.OvertimeMultiplierBps
		}
		if upd.NightShiftMultiplierBps != nil {
			if *upd.NightShiftMultiplierBps < 10000 {
				return invalidPolicyValue("night_shift_multiplier_bps", "must be at least 10000 (1.0x) — cannot pay less than base rate")
			}
			current.NightShiftMultiplierBps = *upd.NightShiftMultiplierBps
		}
		if upd.WeekendShiftMultiplierBps != nil {
			if *upd.WeekendShiftMultiplierBps < 10000 {
				return invalidPolicyValue("weekend_shift_multiplier_bps", "must be at least 10000 (1.0x) — cannot pay less than base rate")
			}
			current.WeekendShiftMultiplierBps = *upd.WeekendShiftMultiplierBps
		}
		if upd.HolidayShiftMultiplierBps != nil {
			if *upd.HolidayShiftMultiplierBps < 10000 {
				return invalidPolicyValue("holiday_shift_multiplier_bps", "must be at least 10000 (1.0x) — cannot pay less than base rate")
			}
			current.HolidayShiftMultiplierBps = *upd.HolidayShiftMultiplierBps
		}

		// Upsert on organization_id (the primary key): payrollPolicyTx hands
		// back an in-memory default when no row exists yet, so this is the
		// same write whether it's the org's first-ever configuration or its
		// hundredth update.
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "organization_id"}},
			UpdateAll: true,
		}).Create(current).Error; err != nil {
			return err
		}
		result = current

		after, err := json.Marshal(current)
		if err != nil {
			return err
		}
		return models.AppendAuditTx(tx, orgID, "PayrollPolicy", orgID, "policy_updated",
			string(before), string(after), actorIP, "")
	})
	return result, err
}
