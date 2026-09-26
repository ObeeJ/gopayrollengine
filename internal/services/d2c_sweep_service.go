package services

import (
	"context"
	"fmt"
	"time"

	"go-payroll-engine/internal/integrations/banklink"
	"go-payroll-engine/internal/models"

	"gorm.io/gorm"
)

// d2cTransactionLookback is how far back GetTransactions is asked to look
// when re-predicting a payday for a sweep — six months comfortably covers
// PredictNextPayday's widest plausible cadence (maxIntervalDays) with room
// for several occurrences, without asking a provider for a worker's entire
// history every run.
const d2cTransactionLookback = -6 // months

// D2CSweepResult is one advance's outcome from a single SweepD2CCollections
// run. Err covers every kind of miss — no linked account, no mandate, no
// confident prediction, a rejected debit submission — not just a genuine
// debit failure (that outcome arrives later, via webhook, as
// ConfirmD2CCollectionFailure; this only reports whether a collection
// attempt was successfully SUBMITTED). Collection is nil whenever nothing
// was due yet or Err is set.
type D2CSweepResult struct {
	AdvanceID  string
	Collection *models.D2CDebitCollection
	Err        error
}

// SweepD2CCollections finds every D2C org's Disbursed advances with no
// collection attempt already in flight, re-predicts each worker's payday
// from their linked account's current transaction history, and initiates a
// collection for any advance whose predicted payday has arrived. Meant to
// run once daily (see cmd/api/main.go's "collect-d2c-debits" mode) — the
// same external-cron pattern AccrualSnapshotCollector and ReconciliationJob
// already use.
//
// One advance's failure (no mandate, no confident prediction, a rejected
// submission) never aborts the sweep — every advance gets its own result,
// and the caller decides what to do with a partial run.
func SweepD2CCollections(ctx context.Context, asOf time.Time, debitProvider banklink.DebitProvider) ([]D2CSweepResult, error) {
	var orgIDs []string
	if err := models.DB.Model(&models.Organization{}).
		Where("is_d2c = ?", true).
		Pluck("id", &orgIDs).Error; err != nil {
		return nil, fmt.Errorf("loading D2C organizations failed: %w", err)
	}

	var results []D2CSweepResult
	for _, orgID := range orgIDs {
		var advances []models.EWAAdvance
		err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
			var inFlightAdvanceIDs []string
			if err := tx.Model(&models.D2CDebitCollection{}).
				Where("organization_id = ? AND status = ?", orgID, models.D2CCollectionPending).
				Pluck("advance_id", &inFlightAdvanceIDs).Error; err != nil {
				return err
			}

			// organization_id is filtered explicitly, not left to RLS
			// alone: this loop iterates every D2C org and calls
			// attemptD2CCollectionForAdvance once per (org, advance) pair
			// it finds, so a connection that bypasses RLS (a superuser —
			// exactly what this codebase's own test suite uses; see
			// setupRLSTestRole's comment) would otherwise have every
			// org's "scoped" query return every org's advances, and
			// reprocess each one once per org instead of once, total.
			// RLS still does the real enforcement for the app's own
			// non-superuser role; this is defense in depth for the one
			// query in this file whose scoping mistake would actually
			// multiply, not just leak.
			q := tx.Where("organization_id = ? AND status = ?", orgID, models.AdvanceDisbursed)
			if len(inFlightAdvanceIDs) > 0 {
				q = q.Where("id NOT IN ?", inFlightAdvanceIDs)
			}
			return q.Find(&advances).Error
		})
		if err != nil {
			results = append(results, D2CSweepResult{
				Err: fmt.Errorf("org %s: loading collectible advances failed: %w", orgID, err),
			})
			continue
		}

		for _, adv := range advances {
			results = append(results, attemptD2CCollectionForAdvance(ctx, orgID, adv, asOf, debitProvider))
		}
	}
	return results, nil
}

// attemptD2CCollectionForAdvance re-predicts one advance's worker's payday
// and initiates a collection only if that prediction has arrived —
// PredictNextPayday is re-run fresh every sweep rather than cached, so a
// worker's actual pay cadence drifting is reflected on the very next run.
func attemptD2CCollectionForAdvance(
	ctx context.Context, orgID string, adv models.EWAAdvance, asOf time.Time, debitProvider banklink.DebitProvider,
) D2CSweepResult {
	var link models.D2CBankLink
	var readConsentActive bool
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		if err := tx.Where("employee_id = ? AND status = ?", adv.EmployeeID, models.D2CBankLinkLinked).
			First(&link).Error; err != nil {
			return err
		}
		readConsentActive = models.HasActiveConsent(tx, orgID, adv.EmployeeID, models.ConsentTypeD2CBankLinkRead)
		return nil
	})
	if err != nil {
		return D2CSweepResult{AdvanceID: adv.ID, Err: fmt.Errorf("no linked bank account: %w", err)}
	}
	// Checked before the mandate below too: a worker can withdraw read
	// access on its own (D2CBankLinkHandler.RevokeDebitMandate leaves it
	// alone, but a plain Revoke or an employer-recorded withdrawal via
	// ConsentHandler.RecordConsent doesn't touch DebitMandateRef either) —
	// this sweep must stop re-reading their transaction history the moment
	// that happens, not just at the point of actually debiting.
	if !readConsentActive {
		return D2CSweepResult{AdvanceID: adv.ID, Err: ErrD2CConsentWithdrawn}
	}
	if link.DebitMandateRef == nil {
		return D2CSweepResult{AdvanceID: adv.ID, Err: banklink.ErrNoMandate}
	}

	txs, err := debitProvider.GetTransactions(ctx, link.ProviderAccountRef, asOf.AddDate(0, d2cTransactionLookback, 0))
	if err != nil {
		return D2CSweepResult{AdvanceID: adv.ID, Err: fmt.Errorf("fetching transaction history failed: %w", err)}
	}

	pred, err := PredictNextPayday(txs, asOf)
	if err != nil {
		return D2CSweepResult{AdvanceID: adv.ID, Err: err}
	}
	if pred.NextPredictedDate.After(asOf) {
		// Not due yet — not an error, just nothing to collect today.
		return D2CSweepResult{AdvanceID: adv.ID}
	}

	collection, err := InitiateD2CCollection(ctx, orgID, adv.ID, pred.NextPredictedDate, debitProvider)
	return D2CSweepResult{AdvanceID: adv.ID, Collection: collection, Err: err}
}
