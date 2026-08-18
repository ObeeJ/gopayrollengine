package models

import (
	"errors"
	"fmt"

	"go-payroll-engine/internal/observability"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
)

// CancelAdvance reverses an advance that committed a receivable but never
// actually moved cash — either because it was never disbursed by the time
// payroll settled (SettleAdvancesForPayrollItem), or because a provider that
// initially accepted the transfer later reported it failed (the disbursement
// webhook). Both callers face the identical accounting problem: money was
// provisionally promised and now needs to be taken back off the books.
//
// The reversal is a new entry, never an edit — ledger entries are immutable —
// so it debits cash_settlement and credits advance_receivable, the exact
// mirror of the debit/credit pair RequestAdvance posted when the advance was
// approved.
//
// Returns ErrStaleStatus if a concurrent writer already transitioned the
// advance; callers treat that as idempotent success (webhook) or skip-and-
// continue (a settlement pass iterating several advances), matching how every
// other CAS transition in this codebase is handled.
func CancelAdvance(tx *gorm.DB, adv *EWAAdvance, from AdvanceStatus, reason string) error {
	if err := TransitionAdvance(tx, adv, from, AdvanceCancelled); err != nil {
		return err
	}
	if err := tx.Model(&EWAAdvance{}).Where("id = ?", adv.ID).
		Update("decline_reason", reason).Error; err != nil {
		return err
	}

	receivable, err := EnsureAccount(tx, adv.OrganizationID, adv.EmployeeID, AccountAdvanceReceivable, money.NGN)
	if err != nil {
		return err
	}
	cash, err := EnsureAccount(tx, adv.OrganizationID, "", AccountCashSettlement, money.NGN)
	if err != nil {
		return err
	}

	if _, err := PostTransaction(tx, PostingRequest{
		OrgID:          adv.OrganizationID,
		Kind:           "ewa_cancellation",
		Reference:      adv.ID,
		IdempotencyKey: "ewa_cancellation:" + adv.ID,
		Entries: []EntryInput{
			{AccountID: cash.ID, Direction: Debit, Amount: money.NGNFromKobo(adv.AmountKobo)},
			{AccountID: receivable.ID, Direction: Credit, Amount: money.NGNFromKobo(adv.AmountKobo)},
		},
	}); err != nil {
		observability.LedgerImbalanceTotal.WithLabelValues(adv.OrganizationID).Inc()
		return err
	}

	return AppendAuditTx(tx, adv.OrganizationID, "EWAAdvance", adv.ID, "cancelled", string(from), reason, "internal", "")
}

// ErrAdvanceAlreadySubmitted is returned by MarkSubmittedToProvider when the
// advance already carries a provider reference — the caller asked to submit
// something that has already been sent.
var ErrAdvanceAlreadySubmitted = errors.New("models: advance already submitted to a provider")

// MarkSubmittedToProvider records which provider accepted an advance and that
// provider's own reference for it. Does not transition the FSM: acceptance by
// a provider means the rail has queued the transfer, not that money has
// landed, so the advance stays Approved until the provider's webhook confirms
// it — see CLAUDE.md's note on why Disbursed means confirmed, not submitted.
func MarkSubmittedToProvider(tx *gorm.DB, advanceID, providerName, providerReference string) error {
	res := tx.Model(&EWAAdvance{}).
		Where("id = ? AND provider_reference IS NULL", advanceID).
		Updates(map[string]interface{}{
			"provider_name":      providerName,
			"provider_reference": providerReference,
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: %s", ErrAdvanceAlreadySubmitted, advanceID)
	}
	return nil
}

// ConfirmDisbursed transitions an advance from Approved to Disbursed once a
// provider webhook (or reconciliation poll) confirms the money actually moved.
func ConfirmDisbursed(tx *gorm.DB, adv *EWAAdvance) error {
	if err := TransitionAdvance(tx, adv, AdvanceApproved, AdvanceDisbursed); err != nil {
		return err
	}
	return AppendAuditTx(tx, adv.OrganizationID, "EWAAdvance", adv.ID, "disbursed",
		string(AdvanceApproved), string(AdvanceDisbursed), "internal", "")
}
