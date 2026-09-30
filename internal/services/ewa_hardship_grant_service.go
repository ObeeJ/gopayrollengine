package services

import (
	"context"
	"errors"
	"fmt"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/observability"
	"go-payroll-engine/internal/workers"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
)

// ErrInvalidHardshipGrant is returned when a grant fails validation.
var ErrInvalidHardshipGrant = errors.New("invalid hardship grant")

func invalidHardshipGrant(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidHardshipGrant, reason)
}

// ErrHardshipGrantPoolExhausted is returned when RequireFundingCoverage is
// on and the org has not funded enough headroom to cover the grant — the
// same guardrail RequestAdvance already enforces for advances, applied here
// since a grant draws down the identical shared cash_settlement pool.
var ErrHardshipGrantPoolExhausted = errors.New("hardship grant: employer funding pool exhausted")

// ErrHardshipGrantEmployeeIneligible — the employee isn't in this org or is no longer active.
var ErrHardshipGrantEmployeeIneligible = errors.New("hardship grant: employee not found or not active")

// IssueHardshipGrant records an admin's decision to give a worker money
// outright — a genuine alternative to a fourth advance, not another draw
// against wages. Unlike RequestAdvance, there is no eligibility scoring and
// no receivable: the ledger entry is Dr hardship_grant_expense /
// Cr cash_settlement, an immediate, permanent expense.
//
// The grant starts Pending and is paid by a worker task
// (workers.HardshipGrantDisbursementHandler) enqueued inside this same
// transaction: if the enqueue fails the grant is rolled back and the admin
// can simply retry, so a grant can never be recorded without a payout being
// scheduled. A provider webhook then confirms (Disbursed) or reverses
// (Failed) it — see models.ConfirmGrantDisbursed / models.FailGrant.
//
// Discretionary employer money requires a human decision, the same reason
// employee termination and payroll creation are admin-only — see the
// handler's role gate, not this method, which trusts its caller.
func (s *EWAService) IssueHardshipGrant(
	ctx context.Context, orgID, employeeID string, amount money.Kobo, reason, actorIP string,
) (*models.EWAHardshipGrant, error) {
	if !amount.IsPositive() {
		return nil, invalidHardshipGrant("amount must be positive")
	}
	if reason == "" {
		return nil, invalidHardshipGrant("reason is required")
	}

	var grant *models.EWAHardshipGrant
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		// Same per-org lock RequestAdvance takes: grants and advances draw
		// on one funding pool, so the headroom check below must not
		// interleave with either kind of concurrent draw.
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "ewa_advance:"+orgID).Error; err != nil {
			return err
		}

		// The employee_id foreign key alone would accept another org's
		// employee. RLS already scopes this read; the explicit org filter
		// keeps it correct under a role that bypasses RLS too.
		var emp models.Employee
		if err := tx.First(&emp, "id = ? AND organization_id = ?", employeeID, orgID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrHardshipGrantEmployeeIneligible
			}
			return err
		}
		if !emp.IsActive {
			return ErrHardshipGrantEmployeeIneligible
		}

		policy, err := s.policyTx(tx, orgID)
		if err != nil {
			return err
		}

		currency, err := models.OrgCurrencyTx(tx, orgID)
		if err != nil {
			return err
		}

		if policy.RequireFundingCoverage {
			exposure, err := models.FundingExposure(tx, orgID, currency)
			if err != nil {
				return err
			}
			headroom := money.Zero
			if exposure.Minor < 0 {
				headroom = money.Kobo(-exposure.Minor)
			}
			if amount > headroom {
				return ErrHardshipGrantPoolExhausted
			}
		}

		g := models.EWAHardshipGrant{
			OrganizationID: orgID,
			EmployeeID:     employeeID,
			AmountKobo:     amount,
			Reason:         reason,
			ApprovedByIP:   actorIP,
		}
		if err := tx.Create(&g).Error; err != nil {
			return err
		}

		cash, err := models.EnsureAccount(tx, orgID, "", models.AccountCashSettlement, currency)
		if err != nil {
			return err
		}
		expense, err := models.EnsureAccount(tx, orgID, "", models.AccountHardshipGrantExpense, currency)
		if err != nil {
			return err
		}

		if _, err := models.PostTransaction(tx, models.PostingRequest{
			OrgID:          orgID,
			Kind:           "hardship_grant",
			Reference:      g.ID,
			IdempotencyKey: "hardship_grant:" + g.ID,
			Entries: []models.EntryInput{
				{AccountID: expense.ID, Direction: models.Debit, Amount: money.KoboIn(currency, amount)},
				{AccountID: cash.ID, Direction: models.Credit, Amount: money.KoboIn(currency, amount)},
			},
		}); err != nil {
			observability.LedgerImbalanceTotal.WithLabelValues(orgID).Inc()
			return err
		}

		if err := models.AppendAuditTx(tx, orgID, "EWAHardshipGrant", g.ID, "issued",
			"", reason, actorIP, ""); err != nil {
			return err
		}

		// Last, so nothing after it can roll the grant back once a task
		// exists. If the commit itself fails, the task finds no grant and
		// exhausts its retries harmlessly.
		if err := workers.EnqueueHardshipGrantDisbursement(orgID, g.ID); err != nil {
			return fmt.Errorf("schedule grant payout: %w", err)
		}

		grant = &g
		return nil
	})
	return grant, err
}

// ListHardshipGrants returns an employee's grant history, most recent first.
func (s *EWAService) ListHardshipGrants(ctx context.Context, orgID, employeeID string) ([]models.EWAHardshipGrant, error) {
	var grants []models.EWAHardshipGrant
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		return tx.Where("employee_id = ?", employeeID).
			Order("created_at DESC").Find(&grants).Error
	})
	return grants, err
}
