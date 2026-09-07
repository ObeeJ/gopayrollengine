package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrInvalidPolicyValue is wrapped with the offending field and reason —
// mirrors the DB CHECK constraints in migration 000014 so a bad value fails
// with a clear 400 here rather than a raw constraint violation surfacing
// from the write.
var ErrInvalidPolicyValue = errors.New("policy: invalid value")

func invalidPolicyValue(field, reason string) error {
	return fmt.Errorf("%w: %s %s", ErrInvalidPolicyValue, field, reason)
}

// PolicyUpdate carries only the fields an admin is changing — nil means
// leave as-is. Applied to whatever the org's current policy is (the default,
// if none has ever been saved), so a first-ever configuration and a normal
// update go through the same path.
type PolicyUpdate struct {
	Enabled                *bool
	MaxAccrualPct          *int
	AbsoluteCapKobo        *money.Kobo
	MinDrawKobo            *money.Kobo
	MaxDrawsPerPeriod      *int
	CoolingOffHours        *int
	EmergencyFloorKobo     *money.Kobo
	RequireFundingCoverage *bool
}

// GetPolicy returns the org's EWAPolicy — the default, unmodified, if the
// org has never saved one.
func (s *EWAService) GetPolicy(ctx context.Context, orgID string) (*models.EWAPolicy, error) {
	var policy *models.EWAPolicy
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		p, err := s.policyTx(tx, orgID)
		policy = p
		return err
	})
	return policy, err
}

// UpdatePolicy validates and applies upd to the org's policy, audits the
// before/after, and returns the result. Every EWA eligibility decision from
// this point on reads the saved row (policyTx), so this takes effect on the
// very next request — there is no cache to invalidate.
func (s *EWAService) UpdatePolicy(ctx context.Context, orgID string, upd PolicyUpdate, actorIP string) (*models.EWAPolicy, error) {
	var result *models.EWAPolicy
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		current, err := s.policyTx(tx, orgID)
		if err != nil {
			return err
		}
		before, err := json.Marshal(current)
		if err != nil {
			return err
		}

		if upd.Enabled != nil {
			current.Enabled = *upd.Enabled
		}
		if upd.MaxAccrualPct != nil {
			if *upd.MaxAccrualPct < 1 || *upd.MaxAccrualPct > 100 {
				return invalidPolicyValue("max_accrual_pct", "must be between 1 and 100")
			}
			current.MaxAccrualPct = *upd.MaxAccrualPct
		}
		if upd.AbsoluteCapKobo != nil {
			if !upd.AbsoluteCapKobo.IsPositive() {
				return invalidPolicyValue("absolute_cap_kobo", "must be positive")
			}
			current.AbsoluteCapKobo = *upd.AbsoluteCapKobo
		}
		if upd.MinDrawKobo != nil {
			if !upd.MinDrawKobo.IsPositive() {
				return invalidPolicyValue("min_draw_kobo", "must be positive")
			}
			current.MinDrawKobo = *upd.MinDrawKobo
		}
		if upd.MaxDrawsPerPeriod != nil {
			if *upd.MaxDrawsPerPeriod < 1 {
				return invalidPolicyValue("max_draws_per_period", "must be positive")
			}
			current.MaxDrawsPerPeriod = *upd.MaxDrawsPerPeriod
		}
		if upd.CoolingOffHours != nil {
			if *upd.CoolingOffHours < 0 {
				return invalidPolicyValue("cooling_off_hours", "must not be negative")
			}
			current.CoolingOffHours = *upd.CoolingOffHours
		}
		if upd.EmergencyFloorKobo != nil {
			if upd.EmergencyFloorKobo.IsNegative() {
				return invalidPolicyValue("emergency_floor_kobo", "must not be negative")
			}
			current.EmergencyFloorKobo = *upd.EmergencyFloorKobo
		}
		if upd.RequireFundingCoverage != nil {
			current.RequireFundingCoverage = *upd.RequireFundingCoverage
		}

		// Upsert on organization_id (the primary key): policyTx hands back an
		// in-memory default when no row exists yet, so this is the same
		// write whether it's the org's first-ever configuration or its
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
		return models.AppendAuditTx(tx, orgID, "EWAPolicy", orgID, "policy_updated",
			string(before), string(after), actorIP, "")
	})
	return result, err
}
