package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go-payroll-engine/internal/integrations/banklink"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
)

// ErrD2CAdvanceNotCollectible means the advance named isn't in a state a
// debit collection can be attempted against — not Disbursed, or already
// fully recovered. Distinct from a debit actually failing: this is a
// caller error (attempting to collect the wrong thing), not something a
// worker's bank account did.
var ErrD2CAdvanceNotCollectible = errors.New("d2c collection: advance is not currently collectible")

// InitiateD2CCollection attempts to collect a Disbursed D2C advance's
// outstanding balance via direct debit against the worker's linked,
// debit-authorized bank account. Creates a new D2CDebitCollection row for
// this attempt (attempt numbering, not overwriting — see migration
// 000031's comment), submits the debit, and records what the provider said
// at submission time. Settlement itself does not happen here: acceptance
// means the rail queued the pull, not that funds moved — see
// ConfirmD2CCollectionSuccess/Failure, called once the provider confirms
// the real outcome via webhook, the same acceptance-vs-confirmation split
// EWADisbursementHandler already holds to on the payout side.
//
// scheduledFor should be the predicted payday (services.PredictNextPayday)
// this attempt targets — recorded for audit, not used by this function.
func InitiateD2CCollection(
	ctx context.Context, orgID, advanceID string, scheduledFor time.Time, debitProvider banklink.DebitProvider,
) (*models.D2CDebitCollection, error) {
	var collection *models.D2CDebitCollection
	var mandateRef string
	var amount money.Money

	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		var adv models.EWAAdvance
		if err := tx.First(&adv, "id = ?", advanceID).Error; err != nil {
			return err
		}
		if adv.Status != models.AdvanceDisbursed {
			return ErrD2CAdvanceNotCollectible
		}
		remaining := adv.RemainingKobo()
		if !remaining.IsPositive() {
			return ErrD2CAdvanceNotCollectible
		}

		var link models.D2CBankLink
		if err := tx.Where("employee_id = ? AND status = ?", adv.EmployeeID, models.D2CBankLinkLinked).
			First(&link).Error; err != nil {
			return err
		}
		if link.DebitMandateRef == nil {
			return banklink.ErrNoMandate
		}
		mandateRef = *link.DebitMandateRef

		var lastAttempt int
		if err := tx.Model(&models.D2CDebitCollection{}).
			Where("advance_id = ?", advanceID).
			Select("COALESCE(MAX(attempt_number), 0)").
			Scan(&lastAttempt).Error; err != nil {
			return err
		}

		currency, err := models.OrgCurrencyTx(tx, orgID)
		if err != nil {
			return err
		}
		amount = money.KoboIn(currency, remaining)

		c := models.D2CDebitCollection{
			OrganizationID: orgID,
			EmployeeID:     adv.EmployeeID,
			AdvanceID:      adv.ID,
			AmountKobo:     remaining,
			AttemptNumber:  lastAttempt + 1,
			ScheduledFor:   scheduledFor,
			Status:         models.D2CCollectionPending,
			Provider:       debitProvider.Name(),
		}
		// The partial unique index (one pending attempt per advance) turns a
		// concurrent second sweep into a constraint violation here, not a
		// double debit.
		if err := tx.Create(&c).Error; err != nil {
			return err
		}
		collection = &c
		return nil
	})
	if err != nil {
		return nil, err
	}

	result, submitErr := debitProvider.InitiateDebit(ctx, mandateRef, amount, collection.ID)
	if submitErr != nil || !result.Accepted {
		message := "rail unreachable"
		if submitErr == nil {
			message = result.Message
		}
		failErr := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
			return models.ConfirmD2CCollectionFailure(tx, collection, "submission_rejected: "+message)
		})
		if failErr != nil {
			return nil, fmt.Errorf("debit submission failed (%s) and recording that failure also failed: %w", message, failErr)
		}
		if submitErr != nil {
			return nil, fmt.Errorf("debit submission failed: %w", submitErr)
		}
		return nil, fmt.Errorf("debit submission rejected: %s", result.Message)
	}

	now := time.Now()
	if err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		return tx.Model(&models.D2CDebitCollection{}).
			Where("id = ?", collection.ID).
			Updates(map[string]interface{}{
				"provider_reference": result.ProviderReference,
				"attempted_at":       now,
			}).Error
	}); err != nil {
		return nil, fmt.Errorf("debit accepted but recording its provider reference failed: %w", err)
	}
	collection.ProviderRef = &result.ProviderReference
	collection.AttemptedAt = &now
	return collection, nil
}
