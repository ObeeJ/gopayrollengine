package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-payroll-engine/internal/integrations/provider"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/workers"
	"go-payroll-engine/pkg/money"
	"time"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

// PayrollService — owns payroll business rules; the only direct GORM contact is the RLS-scoped tx.
type PayrollService struct {
	payrollRepo  repository.PayrollRepository
	employeeRepo repository.EmployeeRepository
	ewa          *EWAService
	providers    *provider.Registry
}

// NewPayrollService — wires up the service with its repository dependencies.
func NewPayrollService(pr repository.PayrollRepository, er repository.EmployeeRepository) *PayrollService {
	return &PayrollService{
		payrollRepo:  pr,
		employeeRepo: er,
		ewa:          NewEWAService(),
		providers:    workers.DefaultProviderRegistry(),
	}
}

var (
	// ErrInvalidPeriod — the period isn't exactly PeriodLayout (YYYY-MM).
	ErrInvalidPeriod = errors.New("payroll: period must be YYYY-MM")
	// ErrNoActiveEmployees — nothing to pay.
	ErrNoActiveEmployees = errors.New("payroll: no active employees found for this organization")
	// ErrUnsupportedCurrency — no provider can settle the org's currency.
	ErrUnsupportedCurrency = errors.New("payroll: no provider can settle this organization's currency")
)

// ValidatePeriod accepts only the canonical PeriodLayout form. Round-tripping
// matters, not just parsing: the (organization_id, period) unique index is
// the only thing stopping the same month being paid twice, and it compares
// strings — "2026-09", "2026-9" and "Sep 2026" would each be a fresh period
// to it, paying every salaried employee once per spelling. The same mismatch
// would also leave advances drawn in "2026-09" never recovered by a run
// named anything else, since settlement matches on the period string too.
func ValidatePeriod(period string) error {
	t, err := time.Parse(PeriodLayout, period)
	if err != nil || t.Format(PeriodLayout) != period {
		return fmt.Errorf("%w: got %q", ErrInvalidPeriod, period)
	}
	return nil
}

// CreatePayroll builds and persists the batch under the org's RLS scope, then queues it for the worker.
func (s *PayrollService) CreatePayroll(ctx context.Context, orgID, period string) (*models.Payroll, error) {
	if err := ValidatePeriod(period); err != nil {
		return nil, err
	}

	payroll := models.Payroll{
		OrganizationID: orgID,
		Period:         period,
		Status:         models.PayrollPending,
	}

	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		// Same org-wide lock RequestAdvance takes: settlement below reads
		// each worker's outstanding advances and recovers them from this
		// run's pay, so an advance approved between that read and this
		// transaction's commit would be missed by this run's withholding.
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "ewa_advance:"+orgID).Error; err != nil {
			return err
		}

		// Fail now, synchronously, rather than queuing a run the async worker
		// can only ever fail deep inside a Monnify bulk-transfer call: the
		// worker's disbursement path is Monnify-specific and Monnify only
		// settles NGN, so an org configured with any other currency has no
		// provider that can actually pay this out yet. See provider.Registry's
		// own doc comment — silently falling back to some other currency's
		// rail would move money through one that cannot pay it out.
		currency, err := models.OrgCurrencyTx(tx, orgID)
		if err != nil {
			return err
		}
		if _, err := s.providers.Select(currency); err != nil {
			return fmt.Errorf("%w: %w", ErrUnsupportedCurrency, err)
		}

		employees, err := s.employeeRepo.WithTx(tx).FindAllActive(orgID)
		if err != nil {
			return err
		}
		if len(employees) == 0 {
			return ErrNoActiveEmployees
		}

		txPayrollRepo := s.payrollRepo.WithTx(tx)
		if err := txPayrollRepo.Create(&payroll); err != nil {
			return err
		}

		// Each item is gross pay minus any early wage access already drawn for
		// this period. Netting here — inside the same transaction that settles the
		// advance and posts the ledger entry — is what stops an advance from being
		// money the business never gets back.
		netAmounts := make([]money.Kobo, 0, len(employees))
		for _, emp := range employees {
			gross := emp.Salary
			var hourlyEntryIDs []string
			if emp.IsHourly() {
				// Gross is real approved hours × rate, with shift
				// differentials and weekly overtime applied, never the fixed
				// Salary column, which hourly employees don't use. asOf=now
				// is safe here: payroll normally runs after the period has
				// closed, so the period boundary — not the asOf bound — is
				// what limits how far into the future this reaches. An entry
				// approved after ITS OWN period's payroll already ran is not
				// lost, though: ComputeHourlyGross sweeps any never-paid
				// approved entry regardless of which period it fell in, and
				// MarkTimeEntriesPaid below stamps every entry it used so no
				// later run pays them again.
				breakdown, err := ComputeHourlyGross(tx, orgID, emp.ID, period, time.Now(), emp.HourlyRateKobo)
				if err != nil {
					return fmt.Errorf("hourly gross computation for %s failed: %w", emp.ID, err)
				}
				gross = breakdown.GrossKobo
				hourlyEntryIDs = breakdown.EntryIDs
			}

			item := models.PayrollItem{
				OrganizationID: orgID,
				PayrollID:      payroll.ID,
				EmployeeID:     emp.ID,
				EmployeeName:   emp.Name,
				Amount:         gross,
				Status:         models.PayrollPending,
			}
			if err := txPayrollRepo.CreateItem(&item); err != nil {
				return err
			}
			if err := models.MarkTimeEntriesPaid(tx, orgID, hourlyEntryIDs, item.ID); err != nil {
				return fmt.Errorf("marking time entries paid for %s failed: %w", emp.ID, err)
			}

			// gross caps what settlement may withhold: SettleAdvancesForPayrollItem
			// recovers outstanding advances up to this item's gross pay and, if a
			// worker's total outstanding exceeds it, settles the remainder
			// partially rather than pushing net pay negative — see that
			// function's comment.
			withheld, err := s.ewa.SettleAdvancesForPayrollItem(tx, orgID, emp.ID, period, item.ID, gross)
			if err != nil {
				return fmt.Errorf("advance settlement for %s failed: %w", emp.ID, err)
			}

			net := gross
			if withheld.IsPositive() {
				net, err = gross.Sub(withheld)
				if err != nil {
					return fmt.Errorf("net pay computation for %s failed: %w", emp.ID, err)
				}
				// withheld is capped at gross by construction, so this is
				// unreachable — kept as a belt-and-suspenders check: paying a
				// negative amount would be far worse than stopping the run.
				if net.IsNegative() {
					return fmt.Errorf(
						"employee %s: advances (%s) exceed gross pay (%s) — refusing to build a negative payroll item",
						emp.ID, withheld, gross)
				}
			}

			// Automated savings (worker opt-in, Phase 4 roadmap item) diverts
			// from whatever is left after EWA settlement, never from gross —
			// the worker's own election should never compete with recovering
			// an advance they already drew against this same pay.
			diverted, err := s.ewa.DivertSavingsForPayrollItem(tx, orgID, emp.ID, item.ID, net)
			if err != nil {
				return fmt.Errorf("savings diversion for %s failed: %w", emp.ID, err)
			}
			if diverted.IsPositive() {
				net, err = net.Sub(diverted)
				if err != nil {
					return fmt.Errorf("net pay computation for %s failed: %w", emp.ID, err)
				}
			}

			// The breakdown behind the net, kept on the item so a payslip can
			// show it without reverse-engineering it (migration 000034).
			updates := map[string]interface{}{
				"gross_kobo":             gross,
				"advances_deducted_kobo": withheld,
				"savings_kobo":           diverted,
			}
			if withheld.IsPositive() || diverted.IsPositive() {
				updates["amount"] = net
			}
			// Nothing to pay (net fully withheld, or an hourly worker with no
			// approved hours this period) — settle the item now rather than
			// sending a zero-amount line to the bank, which rails reject and
			// which can fail the entire batch for everyone else in it. The
			// worker only submits PayrollPending items, so this one is
			// neither sent nor counted toward pending_count.
			if !net.IsPositive() {
				updates["status"] = models.PayrollCompleted
			}
			if len(updates) > 0 {
				if err := tx.Model(&models.PayrollItem{}).
					Where("id = ?", item.ID).
					Updates(updates).Error; err != nil {
					return err
				}
			}
			netAmounts = append(netAmounts, net)
		}

		// Overflow-checked fold over Kobo — never use += on money. The batch total
		// is the sum of what will actually be disbursed, not of gross salaries.
		total, sumErr := money.Sum(netAmounts)
		if sumErr != nil {
			return fmt.Errorf("payroll total overflow: %w", sumErr)
		}
		payroll.TotalAmount = total
		return tx.Model(&models.Payroll{}).
			Where("id = ?", payroll.ID).
			Update("total_amount", total).Error
	})
	if err != nil {
		return nil, err
	}

	// Enqueue after commit; failed enqueue leaves status=pending for retry, payload carries orgID for RLS.
	payload, _ := json.Marshal(map[string]string{"payroll_id": payroll.ID, "org_id": orgID})
	// Bounded, not asynq's default of 25: the worker only returns a
	// retryable error when the bank definitively rejected the batch (so
	// nothing was sent and retrying is safe); an ambiguous outcome is
	// returned as asynq.SkipRetry — see ProcessPayrollTask.
	task := asynq.NewTask(workers.TypeProcessPayroll, payload, asynq.MaxRetry(5))
	if _, err := workers.Client.Enqueue(task); err != nil {
		return nil, fmt.Errorf("payroll created but queue rejected it: %w", err)
	}

	return &payroll, nil
}

// GetPayroll — loads a batch with its items, RLS-scoped to the caller's org.
func (s *PayrollService) GetPayroll(ctx context.Context, orgID, id string) (*models.Payroll, error) {
	var payroll *models.Payroll
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		p, fetchErr := s.payrollRepo.WithTx(tx).FindWithItems(orgID, id)
		payroll = p
		return fetchErr
	})
	return payroll, err
}
