package models

import (
	"errors"
	"fmt"
	"time"

	"go-payroll-engine/internal/observability"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
)

// maxD2CCollectionAttempts caps how many predicted-payday debit attempts an
// advance gets before it's written off. Unlike a payroll-funded advance,
// which settles the moment a payroll run happens to occur, a D2C debit
// depends on funds actually being present in an account nobody but the
// worker controls — a few missed paydays is a real, expected outcome, not
// a system failure, and this must not retry forever against a worker who
// cannot pay.
const maxD2CCollectionAttempts = 4

// ErrD2CCollectionAlreadyResolved means the collection row this webhook
// names isn't 'pending' anymore — a concurrent delivery of the same event,
// or a status poll racing a webhook. Callers treat this as idempotent
// success, the same posture ErrStaleStatus gets everywhere else.
var ErrD2CCollectionAlreadyResolved = errors.New("d2c collection: already resolved")

// ConfirmD2CCollectionSuccess records that a direct debit actually landed:
// Dr cash_settlement / Cr advance_receivable, the exact accounting shape
// SettleAdvancesForPayrollItem posts on the payroll-funded path — money
// arrived, the receivable is reduced by that much. If this collection
// clears the advance's full outstanding balance, the advance transitions to
// Settled, same as the payroll path's own terminal state.
func ConfirmD2CCollectionSuccess(tx *gorm.DB, collection *D2CDebitCollection) error {
	res := tx.Model(&D2CDebitCollection{}).
		Where("id = ? AND status = ?", collection.ID, D2CCollectionPending).
		Updates(map[string]interface{}{"status": D2CCollectionSuccessful, "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrD2CCollectionAlreadyResolved
	}
	collection.Status = D2CCollectionSuccessful

	var adv EWAAdvance
	if err := tx.First(&adv, "id = ?", collection.AdvanceID).Error; err != nil {
		return err
	}

	currency, err := OrgCurrencyTx(tx, collection.OrganizationID)
	if err != nil {
		return err
	}
	cash, err := EnsureAccount(tx, collection.OrganizationID, "", AccountCashSettlement, currency)
	if err != nil {
		return err
	}
	receivable, err := EnsureAccount(tx, collection.OrganizationID, collection.EmployeeID, AccountAdvanceReceivable, currency)
	if err != nil {
		return err
	}

	if _, err := PostTransaction(tx, PostingRequest{
		OrgID:          collection.OrganizationID,
		Kind:           "d2c_debit_collection",
		Reference:      collection.ID,
		IdempotencyKey: "d2c_collection:" + collection.ID,
		Entries: []EntryInput{
			{AccountID: cash.ID, Direction: Debit, Amount: money.KoboIn(currency, collection.AmountKobo)},
			{AccountID: receivable.ID, Direction: Credit, Amount: money.KoboIn(currency, collection.AmountKobo)},
		},
	}); err != nil {
		observability.LedgerImbalanceTotal.WithLabelValues(collection.OrganizationID).Inc()
		return err
	}

	recoveredTotal, err := adv.RecoveredKobo.Add(collection.AmountKobo)
	if err != nil {
		return fmt.Errorf("recovered total overflow: %w", err)
	}
	updRes := tx.Model(&EWAAdvance{}).
		Where("id = ? AND recovered_kobo = ?", adv.ID, adv.RecoveredKobo).
		Update("recovered_kobo", recoveredTotal)
	if updRes.Error != nil {
		return updRes.Error
	}
	if updRes.RowsAffected == 0 {
		return fmt.Errorf("%w: advance %s recovered_kobo changed concurrently", ErrStaleStatus, adv.ID)
	}
	adv.RecoveredKobo = recoveredTotal

	if adv.RecoveredKobo >= adv.AmountKobo {
		if err := TransitionAdvance(tx, &adv, AdvanceDisbursed, AdvanceSettled); err != nil {
			if errors.Is(err, ErrStaleStatus) {
				return nil // a concurrent path already settled it
			}
			return err
		}
		return AppendAuditTx(tx, collection.OrganizationID, "EWAAdvance", adv.ID, "settled_via_d2c_debit",
			string(AdvanceDisbursed), string(AdvanceSettled), "internal", "")
	}
	return AppendAuditTx(tx, collection.OrganizationID, "D2CDebitCollection", collection.ID, "collected",
		"", collection.AmountKobo.String(), "internal", "")
}

// ConfirmD2CCollectionFailure records that a debit attempt did not land —
// insufficient funds, a revoked mandate, a closed account. This is an
// ordinary, expected outcome, not a system error: the advance stays
// Disbursed so a later predicted payday can retry it, unless
// maxD2CCollectionAttempts has been reached, at which point the advance is
// written off exactly the way a terminated employee's undischarged advance
// already is — the same unrecoverable-loss treatment, a different reason
// for reaching it.
func ConfirmD2CCollectionFailure(tx *gorm.DB, collection *D2CDebitCollection, reason string) error {
	res := tx.Model(&D2CDebitCollection{}).
		Where("id = ? AND status = ?", collection.ID, D2CCollectionPending).
		Updates(map[string]interface{}{"status": D2CCollectionFailed, "updated_at": time.Now()})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return ErrD2CCollectionAlreadyResolved
	}
	collection.Status = D2CCollectionFailed

	if err := AppendAuditTx(tx, collection.OrganizationID, "D2CDebitCollection", collection.ID, "collection_failed",
		"", reason, "internal", ""); err != nil {
		return err
	}

	var failedAttempts int64
	if err := tx.Model(&D2CDebitCollection{}).
		Where("advance_id = ? AND status = ?", collection.AdvanceID, D2CCollectionFailed).
		Count(&failedAttempts).Error; err != nil {
		return err
	}
	if failedAttempts < maxD2CCollectionAttempts {
		return nil
	}

	var adv EWAAdvance
	if err := tx.First(&adv, "id = ?", collection.AdvanceID).Error; err != nil {
		return err
	}
	if adv.Status != AdvanceDisbursed {
		return nil // already resolved by some other path
	}
	if err := WriteOffAdvance(tx, &adv, "d2c_debit_collection_exhausted"); err != nil {
		if errors.Is(err, ErrStaleStatus) {
			return nil
		}
		return err
	}
	return nil
}
