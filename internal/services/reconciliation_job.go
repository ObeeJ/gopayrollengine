package services

import (
	"context"
	"errors"
	"log"
	"os"

	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/observability"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
)

// ReconciliationJob compares the ledger's implied cash position against the
// Monnify disbursement wallet's actual balance — Phase 3's "ledger balances
// vs. Monnify statements, alerting on drift."
//
// WHY A DELTA, NOT AN ABSOLUTE COMPARISON
// There is one shared Monnify source wallet across every org (see
// MONNIFY_SOURCE_WALLET); an individual org's cash_settlement balance is an
// accounting allocation of that shared pool, not a real bank balance with
// its own number to check against, and this job has no record of the
// wallet's starting float when the platform began operating — so there is
// no absolute figure to compare the wallet balance to. What IS checkable:
// cash_settlement (Credit-normal) is "advances disbursed minus employer
// deposits" — every naira it grows by is a naira that had to leave the
// wallet to fund a draw, and every naira it shrinks by is a naira an
// employer deposit put back. So between two runs, the wallet balance should
// move by exactly the negative of however much total cash_settlement moved.
// Any other movement in the wallet (a manual admin transfer, an unrelated
// payroll disbursement, a Monnify-side fee) shows up as drift — which is
// the point: this job cannot distinguish "a bug" from "an untracked
// movement", only surface that ledger and wallet disagree and by how much.
type ReconciliationJob struct {
	monnify       *monnify.Client
	thresholdKobo money.Kobo
}

// NewReconciliationJob wires up the job. threshold is the absolute drift
// (Kobo) beyond which a run is flagged.
func NewReconciliationJob(client *monnify.Client, threshold money.Kobo) *ReconciliationJob {
	return &ReconciliationJob{monnify: client, thresholdKobo: threshold}
}

// Run performs one reconciliation pass and persists it. The first-ever run
// has nothing to diff against and always has a nil DriftKobo — that is not
// an alert, it is the job establishing its baseline.
func (j *ReconciliationJob) Run(ctx context.Context) (*models.ReconciliationRun, error) {
	walletNumber := os.Getenv("MONNIFY_SOURCE_WALLET")
	walletBalance, err := j.monnify.GetWalletBalance(walletNumber)
	if err != nil {
		return nil, err
	}

	totalCashSettlement, err := j.sumCashSettlement(ctx)
	if err != nil {
		return nil, err
	}

	run := &models.ReconciliationRun{
		WalletBalanceKobo:       walletBalance,
		TotalCashSettlementKobo: totalCashSettlement,
	}

	var prev models.ReconciliationRun
	err = models.DB.Order("run_at DESC").First(&prev).Error
	switch {
	case err == nil:
		walletDelta, addErr := walletBalance.Sub(prev.WalletBalanceKobo)
		if addErr != nil {
			return nil, addErr
		}
		cashDelta, addErr := totalCashSettlement.Sub(prev.TotalCashSettlementKobo)
		if addErr != nil {
			return nil, addErr
		}
		drift, addErr := walletDelta.Add(cashDelta)
		if addErr != nil {
			return nil, addErr
		}
		run.DriftKobo = &drift

		absDrift := drift
		if absDrift.IsNegative() {
			absDrift = -absDrift
		}
		if absDrift > j.thresholdKobo {
			run.Alerted = true
			observability.ReconciliationAlertsTotal.Inc()
			log.Printf("CRITICAL: reconciliation drift %s exceeds threshold %s (wallet=%s, cash_settlement=%s)", //nolint:gosec // all four values are internally computed money.Kobo (int64); no attacker-controlled input reaches this format string
				drift, j.thresholdKobo, walletBalance, totalCashSettlement)
		}
		observability.ReconciliationDriftKobo.Set(float64(drift))
	case errors.Is(err, gorm.ErrRecordNotFound):
		// First-ever run: no baseline to diff against.
	default:
		return nil, err
	}

	if err := models.DB.Create(run).Error; err != nil {
		return nil, err
	}
	return run, nil
}

// sumCashSettlement totals every org's cash_settlement ledger balance.
// organizations has no RLS (the tenant root), so listing every org is
// unrestricted; ledger_accounts does, so each org's own balance is read
// inside its own WithOrgScope transaction.
func (j *ReconciliationJob) sumCashSettlement(ctx context.Context) (money.Kobo, error) {
	var orgs []models.Organization
	if err := models.DB.Find(&orgs).Error; err != nil {
		return 0, err
	}

	var total money.Kobo
	for _, org := range orgs {
		err := models.WithOrgScope(ctx, org.ID, func(tx *gorm.DB) error {
			acct, err := models.EnsureAccount(tx, org.ID, "", models.AccountCashSettlement, money.NGN)
			if err != nil {
				return err
			}
			bal, err := models.AccountBalance(tx, acct.ID)
			if err != nil {
				return err
			}
			sum, err := total.Add(money.Kobo(bal.Minor))
			if err != nil {
				return err
			}
			total = sum
			return nil
		})
		if err != nil {
			log.Printf("reconciliation: org %s cash_settlement read failed: %v", org.ID, err)
		}
	}
	return total, nil
}
