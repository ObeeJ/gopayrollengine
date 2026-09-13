package services

import (
	"context"
	"errors"
	"fmt"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/observability"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ErrInvalidSavingsPreference is returned when a savings election fails
// validation — the DB CHECK constraints are the ultimate authority, but this
// gives the API a typed, explainable error instead of a raw SQL failure.
var ErrInvalidSavingsPreference = errors.New("invalid savings preference")

func invalidSavingsPreference(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidSavingsPreference, reason)
}

// SavingsPreferenceUpdate is the worker-facing election. Amount is
// interpreted according to Mode: whole percentage points for FixedPercent,
// Kobo for RoundUp.
type SavingsPreferenceUpdate struct {
	Enabled bool
	Mode    models.SavingsMode
	Amount  int64
}

// maxFixedSavingsPercent mirrors the DB CHECK in migration 000023 — kept
// here too so a bad request is rejected before it ever reaches the database,
// with an explanation the worker-facing API can return.
const maxFixedSavingsPercent = 90

// SetSavingsPreference validates and upserts a worker's automated-savings
// election. Opt-in and worker-controlled, exactly like SetProtectedPayday:
// the system diverts money at the worker's own instruction, never on its
// own judgement about what they should be saving.
func (s *EWAService) SetSavingsPreference(
	ctx context.Context, orgID, employeeID string, upd SavingsPreferenceUpdate,
) (*models.EWASavingsPreference, error) {
	if upd.Enabled {
		switch upd.Mode {
		case models.SavingsFixedPercent:
			if upd.Amount <= 0 || upd.Amount > maxFixedSavingsPercent {
				return nil, invalidSavingsPreference(
					fmt.Sprintf("fixed_percent must be between 1 and %d", maxFixedSavingsPercent))
			}
		case models.SavingsRoundUp:
			if upd.Amount <= 0 {
				return nil, invalidSavingsPreference("round_up_to_kobo must be positive")
			}
		default:
			return nil, invalidSavingsPreference("mode must be 'fixed_percent' or 'round_up'")
		}
	}

	pref := models.EWASavingsPreference{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		Enabled:        upd.Enabled,
		Mode:           upd.Mode,
	}
	switch upd.Mode {
	case models.SavingsFixedPercent:
		pref.FixedPercent = int(upd.Amount)
	case models.SavingsRoundUp:
		pref.RoundUpToKobo = money.Kobo(upd.Amount)
	}

	var result *models.EWASavingsPreference
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		// Upsert, not tx.Save: the primary key is composite and a worker's
		// first-ever election would otherwise silently affect zero rows — see
		// SetProtectedPayday's identical reasoning.
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "organization_id"}, {Name: "employee_id"}},
			DoUpdates: clause.AssignmentColumns(
				[]string{"enabled", "mode", "fixed_percent", "round_up_to_kobo", "updated_at"}),
		}).Create(&pref).Error; err != nil {
			return err
		}
		result = &pref
		return nil
	})
	return result, err
}

// GetSavingsPreference reads a worker's election, defaulting to disabled
// when none has ever been set.
func (s *EWAService) GetSavingsPreference(
	ctx context.Context, orgID, employeeID string,
) (*models.EWASavingsPreference, error) {
	var result *models.EWASavingsPreference
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		pref, err := s.savingsPreferenceTx(tx, orgID, employeeID)
		result = pref
		return err
	})
	return result, err
}

func (s *EWAService) savingsPreferenceTx(
	tx *gorm.DB, orgID, employeeID string,
) (*models.EWASavingsPreference, error) {
	var pref models.EWASavingsPreference
	err := tx.First(&pref, "organization_id = ? AND employee_id = ?", orgID, employeeID).Error
	if err == nil {
		return &pref, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	return &models.EWASavingsPreference{OrganizationID: orgID, EmployeeID: employeeID}, nil
}

// GetSavingsBalance returns how much a worker has saved to date.
func (s *EWAService) GetSavingsBalance(ctx context.Context, orgID, employeeID string) (money.Kobo, error) {
	var balance money.Kobo
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		acct, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountEmployeeSavings, money.NGN)
		if err != nil {
			return err
		}
		bal, err := models.AccountBalance(tx, acct.ID)
		if err != nil {
			return err
		}
		balance = money.Kobo(bal.Minor)
		return nil
	})
	return balance, err
}

// DivertSavingsForPayrollItem diverts part of a worker's net pay (post-EWA
// settlement) into their savings balance, per their own election. Returns
// the amount diverted, which the caller must subtract from what actually
// gets disbursed — mirrors SettleAdvancesForPayrollItem's contract exactly.
//
// The diverted amount never reaches or exceeds netPay: fixed_percent is
// capped below 100 at election time (see maxFixedSavingsPercent), and
// round-up mode is skipped entirely for a run where the round-up delta would
// consume the whole paycheck — automated savings must never leave a worker
// with nothing on payday, the same non-negotiable the EWA guardrails hold to.
func (s *EWAService) DivertSavingsForPayrollItem(
	tx *gorm.DB, orgID, employeeID, payrollItemID string, netPay money.Kobo,
) (money.Kobo, error) {
	if !netPay.IsPositive() {
		return money.Zero, nil
	}
	pref, err := s.savingsPreferenceTx(tx, orgID, employeeID)
	if err != nil {
		return 0, err
	}
	if !pref.Enabled {
		return money.Zero, nil
	}

	var amount money.Kobo
	switch pref.Mode {
	case models.SavingsFixedPercent:
		if pref.FixedPercent <= 0 {
			return money.Zero, nil
		}
		amount, err = netPay.Percent(int64(pref.FixedPercent), 100)
		if err != nil {
			return 0, err
		}
	case models.SavingsRoundUp:
		if !pref.RoundUpToKobo.IsPositive() {
			return money.Zero, nil
		}
		amount = roundUpDelta(netPay, pref.RoundUpToKobo)
	default:
		return money.Zero, nil
	}

	if !amount.IsPositive() || amount >= netPay {
		return money.Zero, nil
	}

	payable, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountWagePayable, money.NGN)
	if err != nil {
		return 0, err
	}
	savings, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountEmployeeSavings, money.NGN)
	if err != nil {
		return 0, err
	}

	if _, err := models.PostTransaction(tx, models.PostingRequest{
		OrgID:          orgID,
		Kind:           "ewa_savings_diversion",
		Reference:      employeeID,
		IdempotencyKey: "ewa_savings:" + payrollItemID,
		Entries: []models.EntryInput{
			{AccountID: payable.ID, Direction: models.Debit, Amount: money.NGNFromKobo(amount)},
			{AccountID: savings.ID, Direction: models.Credit, Amount: money.NGNFromKobo(amount)},
		},
	}); err != nil {
		observability.LedgerImbalanceTotal.WithLabelValues(orgID).Inc()
		return 0, err
	}
	return amount, nil
}

// roundUpDelta is how far netPay falls short of the next multiple of unit —
// zero if netPay already lands exactly on one.
func roundUpDelta(netPay, unit money.Kobo) money.Kobo {
	if !unit.IsPositive() {
		return money.Zero
	}
	remainder := int64(netPay) % int64(unit)
	if remainder == 0 {
		return money.Zero
	}
	return money.Kobo(int64(unit) - remainder)
}
