package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/workers"
	"go-payroll-engine/pkg/money"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

// Actor identifies who performed an action, for the audit trail. Employer JWTs
// carry a role, not (yet) a person, so Name is typically the role.
type Actor struct {
	Name string
	IP   string
}

var (
	// ErrPayrollNotRetryable — only a batch in 'failed' can be retried.
	ErrPayrollNotRetryable = errors.New("payroll: only a failed batch can be retried")
	// ErrNothingToRetry — no failed line can be retried (none failed, or every
	// failed line belongs to someone who is no longer employed).
	ErrNothingToRetry = errors.New("payroll: there are no failed payments that can be retried")
	// ErrItemNotResolvable — the item is not waiting on the bank.
	ErrItemNotResolvable = errors.New("payroll: this payment is not awaiting a result")
	// ErrNotYetStuck — the callback may still be on its way.
	ErrNotYetStuck = errors.New("payroll: the bank's result is not overdue yet")
	// ErrQueueUnavailable — the change was recorded but could not be queued;
	// calling the same action again resumes it.
	ErrQueueUnavailable = errors.New("payroll: recorded, but the job queue is unavailable")
	// ErrInvalidResolution — missing reason or evidence, or an outcome that isn't paid/failed.
	ErrInvalidResolution = errors.New("payroll: a resolution needs an outcome of paid or failed and a reason")
)

// SkippedItem is a failed line a retry deliberately did not resend.
type SkippedItem struct {
	EmployeeID   string `json:"employee_id"`
	EmployeeName string `json:"employee_name"`
	Reason       string `json:"reason"`
}

// RetryResult says what a retry did.
type RetryResult struct {
	PayrollID string        `json:"payroll_id"`
	Retrying  int           `json:"retrying"`
	Skipped   []SkippedItem `json:"skipped"`
}

// lockPayroll serialises retries, resolutions and settlements on one batch for
// the rest of the transaction.
func lockPayroll(tx *gorm.DB, payrollID string) error {
	return tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "payroll:"+payrollID).Error
}

// RetryFailedItems sends a batch's failed lines to the bank again.
//
// Each retried line gets a new attempt number, and therefore a new bank
// reference (models.PayrollItem.ItemReference): a provider may treat the
// reference as an idempotency key and answer a re-submission of a failed one
// with the original failure. Lines already paid are never touched. A line
// whose employee has since left is skipped, not paid.
//
// It also resumes a batch whose retry was committed but never queued (the
// enqueue after commit can fail): a 'failed' batch with pending lines just
// gets its task enqueued again.
func (s *PayrollService) RetryFailedItems(ctx context.Context, orgID, payrollID string, by Actor) (*RetryResult, error) {
	result := &RetryResult{PayrollID: payrollID, Skipped: []SkippedItem{}}
	var nothing bool

	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		if err := lockPayroll(tx, payrollID); err != nil {
			return err
		}
		repo := s.payrollRepo.WithTx(tx)
		payroll, err := repo.FindByID(orgID, payrollID)
		if err != nil {
			return err
		}
		if payroll.Status != models.PayrollFailed {
			return ErrPayrollNotRetryable
		}

		failed, err := repo.FindItemsByStatus(orgID, payrollID, models.PayrollFailed)
		if err != nil {
			return err
		}

		var retry []string
		if len(failed) > 0 {
			ids := make([]string, 0, len(failed))
			for _, it := range failed {
				ids = append(ids, it.EmployeeID)
			}
			var emps []models.Employee
			if err := tx.Where("organization_id = ? AND id IN ?", orgID, ids).Find(&emps).Error; err != nil {
				return err
			}
			active := make(map[string]bool, len(emps))
			for _, e := range emps {
				active[e.ID] = e.IsActive
			}
			for _, it := range failed {
				if !active[it.EmployeeID] {
					result.Skipped = append(result.Skipped, SkippedItem{
						EmployeeID: it.EmployeeID, EmployeeName: it.EmployeeName,
						Reason: "employee is no longer active",
					})
					continue
				}
				retry = append(retry, it.ID)
			}
		}

		if len(retry) > 0 {
			res := tx.Exec(
				`UPDATE payroll_items
				    SET status = ?, attempt = attempt + 1, error_message = '',
				        sent_at = NULL, settled_at = NULL, updated_at = NOW()
				  WHERE id IN ? AND status = ?`,
				models.PayrollPending, retry, models.PayrollFailed)
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != int64(len(retry)) {
				return fmt.Errorf("payroll %s: %d of %d failed lines changed under the retry: %w",
					payrollID, int64(len(retry))-res.RowsAffected, len(retry), models.ErrStaleStatus)
			}
		}

		// Lines already waiting to be sent: either we just released them, or an
		// earlier retry committed and its task was never queued.
		var waiting int64
		if err := tx.Model(&models.PayrollItem{}).
			Where("payroll_id = ? AND status = ?", payrollID, models.PayrollPending).
			Count(&waiting).Error; err != nil {
			return err
		}
		if waiting == 0 {
			nothing = true
			return nil // nothing changed; leave no audit row
		}
		result.Retrying = int(waiting)

		return models.AppendAuditTx(tx, orgID, "Payroll", payrollID, "retry_failed",
			fmt.Sprintf("%d failed", len(failed)),
			fmt.Sprintf("%d resent, %d skipped", result.Retrying, len(result.Skipped)), by.IP, by.Name)
	})
	if err != nil {
		return nil, err
	}
	if nothing {
		return result, ErrNothingToRetry
	}

	// After commit, like CreatePayroll. A failure here leaves a failed batch
	// with pending lines, which the next call resumes.
	payload, _ := json.Marshal(map[string]string{"payroll_id": payrollID, "org_id": orgID})
	if _, err := workers.Client.Enqueue(asynq.NewTask(workers.TypeProcessPayroll, payload), asynq.MaxRetry(5)); err != nil {
		return nil, fmt.Errorf("%w (call retry again to resume): %w", ErrQueueUnavailable, err)
	}
	return result, nil
}

// PayrollView is a batch with its items and the derived outcome.
type PayrollView struct {
	models.Payroll
	Summary models.PayrollSummary `json:"summary"`
}

// PayrollListEntry is a batch in a list: no items, to keep the page small.
type PayrollListEntry struct {
	ID           string                `json:"id"`
	Period       string                `json:"period"`
	Status       models.PayrollStatus  `json:"status"`
	TotalAmount  money.Kobo            `json:"total_amount"`
	PendingCount int                   `json:"pending_count"`
	CreatedAt    time.Time             `json:"created_at"`
	UpdatedAt    time.Time             `json:"updated_at"`
	Summary      models.PayrollSummary `json:"summary"`
}

// GetPayrollView loads a batch with its items and outcome.
func (s *PayrollService) GetPayrollView(ctx context.Context, orgID, id string) (*PayrollView, error) {
	p, err := s.GetPayroll(ctx, orgID, id)
	if err != nil {
		return nil, err
	}
	return &PayrollView{Payroll: *p, Summary: models.SummarizePayroll(*p, p.Items, time.Now())}, nil
}

// ListPayrolls returns the org's batches newest first. Items are loaded for
// every batch on the page in a single query to derive each outcome.
func (s *PayrollService) ListPayrolls(ctx context.Context, orgID string, page, pageSize int) ([]PayrollListEntry, int64, error) {
	var entries []PayrollListEntry
	var total int64
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		repo := s.payrollRepo.WithTx(tx)
		payrolls, count, err := repo.ListPaginated(orgID, page, pageSize)
		if err != nil {
			return err
		}
		total = count
		ids := make([]string, 0, len(payrolls))
		for _, p := range payrolls {
			ids = append(ids, p.ID)
		}
		items, err := repo.ItemsForPayrolls(orgID, ids)
		if err != nil {
			return err
		}
		byBatch := make(map[string][]models.PayrollItem, len(payrolls))
		for _, it := range items {
			byBatch[it.PayrollID] = append(byBatch[it.PayrollID], it)
		}
		now := time.Now()
		entries = make([]PayrollListEntry, 0, len(payrolls))
		for _, p := range payrolls {
			entries = append(entries, PayrollListEntry{
				ID: p.ID, Period: p.Period, Status: p.Status, TotalAmount: p.TotalAmount,
				PendingCount: p.PendingCount, CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
				Summary: models.SummarizePayroll(p, byBatch[p.ID], now),
			})
		}
		return nil
	})
	return entries, total, err
}

// ResolveOutcome is what an admin attests happened to a payout.
type ResolveOutcome string

const (
	ResolvePaid   ResolveOutcome = "paid"
	ResolveFailed ResolveOutcome = "failed"
)

// ResolveItem settles a payout whose callback never arrived, on an admin's
// word. It exists because callbacks can be lost and the provider's status API
// is not available to us: the alternative is a batch stuck 'processing' for
// ever. Safeguards:
//   - only an item that has been with the bank longer than models.StuckAfter,
//     so a callback still in flight can't be pre-empted by a guess;
//   - a reason is mandatory, an evidence reference is recorded when given;
//   - it applies the same transitions as a callback (SettlePayrollItem), and a
//     callback that finally turns up afterwards changes nothing;
//   - it is audited with who, why and the evidence.
//
// Resolving as 'failed' makes the line retryable (RetryFailedItems).
func (s *PayrollService) ResolveItem(
	ctx context.Context, orgID, payrollID, itemID string, outcome ResolveOutcome, note, evidence string, by Actor,
) (*models.PayrollItem, error) {
	note, evidence = strings.TrimSpace(note), strings.TrimSpace(evidence)
	var next models.PayrollStatus
	switch outcome {
	case ResolvePaid:
		next = models.PayrollCompleted
	case ResolveFailed:
		next = models.PayrollFailed
	default:
		return nil, ErrInvalidResolution
	}
	if note == "" {
		return nil, ErrInvalidResolution
	}
	// "failed" is what makes a line retryable, and retrying a payment that in
	// fact succeeded pays the worker twice. Of the two attestations it is the
	// one that can lose real money, so it must point at something: the
	// provider's statement or transaction lookup showing no transfer.
	if outcome == ResolveFailed && evidence == "" {
		return nil, fmt.Errorf("%w: marking a payment failed needs evidence (e.g. the provider's statement showing "+
			"no transfer), because retrying a payment that actually succeeded pays the worker twice", ErrInvalidResolution)
	}

	var resolved models.PayrollItem
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		if err := lockPayroll(tx, payrollID); err != nil {
			return err
		}
		item, err := s.payrollRepo.WithTx(tx).FindItem(orgID, payrollID, itemID)
		if err != nil {
			return err
		}
		if item.Status != models.PayrollProcessing {
			return ErrItemNotResolvable
		}
		since := item.UpdatedAt
		if item.SentAt != nil {
			since = *item.SentAt
		}
		if time.Since(since) <= models.StuckAfter {
			return ErrNotYetStuck
		}

		applied, err := SettlePayrollItem(tx, orgID, item, next, SettlementSource{
			IP: by.IP, Actor: by.Name, Note: note, Evidence: evidence,
		})
		if err != nil {
			return err
		}
		if !applied {
			return ErrItemNotResolvable
		}
		return tx.First(&resolved, "id = ?", itemID).Error
	})
	if err != nil {
		return nil, err
	}
	return &resolved, nil
}
