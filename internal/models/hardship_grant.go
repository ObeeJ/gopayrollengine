package models

import (
	"fmt"
	"time"

	"go-payroll-engine/internal/observability"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
)

// MarkGrantSubmitted records that a provider accepted a pending grant's
// transfer. Acceptance is not confirmation — the grant becomes Disbursed
// only when the provider's webhook says the money landed.
//
// Returns ErrStaleStatus if the grant is no longer pending (a concurrent
// worker already recorded a submission, or a webhook already resolved it).
func MarkGrantSubmitted(tx *gorm.DB, grantID, providerName, providerReference string) error {
	res := tx.Model(&EWAHardshipGrant{}).
		Where("id = ? AND status = ?", grantID, GrantPending).
		Updates(map[string]interface{}{
			"status":             GrantSubmitted,
			"provider_name":      providerName,
			"provider_reference": providerReference,
			"submitted_at":       time.Now(),
		})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: grant %s", ErrStaleStatus, grantID)
	}
	return nil
}

// ConfirmGrantDisbursed moves a pending or submitted grant to Disbursed once
// the provider confirms the transfer. A webhook can beat the worker's own
// MarkGrantSubmitted write, so pending is accepted too.
func ConfirmGrantDisbursed(tx *gorm.DB, g *EWAHardshipGrant) error {
	now := time.Now()
	res := tx.Model(&EWAHardshipGrant{}).
		Where("id = ? AND status IN ?", g.ID, []HardshipGrantStatus{GrantPending, GrantSubmitted}).
		Updates(map[string]interface{}{"status": GrantDisbursed, "disbursed_at": now})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: grant %s", ErrStaleStatus, g.ID)
	}
	from := g.Status
	g.Status, g.DisbursedAt = GrantDisbursed, &now
	return AppendAuditTx(tx, g.OrganizationID, "EWAHardshipGrant", g.ID, "disbursed",
		string(from), string(GrantDisbursed), "internal", "")
}

// FailGrant marks a pending or submitted grant Failed and reverses its
// ledger entry. Cash never left, so the reversal is the exact mirror of what
// IssueHardshipGrant posted — Dr cash_settlement / Cr hardship_grant_expense
// — as a new entry, never an edit.
func FailGrant(tx *gorm.DB, g *EWAHardshipGrant, reason string) error {
	res := tx.Model(&EWAHardshipGrant{}).
		Where("id = ? AND status IN ?", g.ID, []HardshipGrantStatus{GrantPending, GrantSubmitted}).
		Updates(map[string]interface{}{"status": GrantFailed, "failure_reason": reason})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: grant %s", ErrStaleStatus, g.ID)
	}
	from := g.Status
	g.Status, g.FailureReason = GrantFailed, &reason

	currency, err := OrgCurrencyTx(tx, g.OrganizationID)
	if err != nil {
		return err
	}
	cash, err := EnsureAccount(tx, g.OrganizationID, "", AccountCashSettlement, currency)
	if err != nil {
		return err
	}
	expense, err := EnsureAccount(tx, g.OrganizationID, "", AccountHardshipGrantExpense, currency)
	if err != nil {
		return err
	}
	if _, err := PostTransaction(tx, PostingRequest{
		OrgID:          g.OrganizationID,
		Kind:           "hardship_grant_reversal",
		Reference:      g.ID,
		IdempotencyKey: "hardship_grant_reversal:" + g.ID,
		Entries: []EntryInput{
			{AccountID: cash.ID, Direction: Debit, Amount: money.KoboIn(currency, g.AmountKobo)},
			{AccountID: expense.ID, Direction: Credit, Amount: money.KoboIn(currency, g.AmountKobo)},
		},
	}); err != nil {
		observability.LedgerImbalanceTotal.WithLabelValues(g.OrganizationID).Inc()
		return err
	}
	return AppendAuditTx(tx, g.OrganizationID, "EWAHardshipGrant", g.ID, "failed", string(from), reason, "internal", "")
}
