package workers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/observability"
	"log"
	"os"
	"time"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

type PayrollHandler struct {
	MonnifyClient *monnify.Client
}

// NewPayrollHandler — wires up the Monnify client; MOCK_MODE=true makes it pretend convincingly.
func NewPayrollHandler() *PayrollHandler {
	return &PayrollHandler{MonnifyClient: monnify.NewClient()}
}

// ProcessPayrollTask — the worker that actually moves money; treat every line here with respect.
func (h *PayrollHandler) ProcessPayrollTask(ctx context.Context, t *asynq.Task) error {
	start := time.Now()
	var payload map[string]string
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		observability.WorkerTasksTotal.WithLabelValues(TypeProcessPayroll, "error").Inc()
		return err
	}

	payrollID := payload["payroll_id"]
	orgID := payload["org_id"]
	log.Printf("Processing payroll %s for org %s", payrollID, orgID)

	// Phase 1 — load, partition, CAS-transition, set counter, fetch employees in one RLS-scoped tx.
	var payroll models.Payroll
	var employees []models.Employee
	var sendable []models.PayrollItem

	if err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		if err := tx.Preload("Items").First(&payroll, "id = ?", payrollID).Error; err != nil {
			return err
		}

		// Only items that have NOT reached a terminal state may be submitted. The
		// failed→processing retry edge means a batch can re-enter this worker while
		// carrying items Monnify already settled; re-sending those disburses twice.
		unsettled := make([]models.PayrollItem, 0, len(payroll.Items))
		for _, item := range payroll.Items {
			if item.Status == models.PayrollPending {
				unsettled = append(unsettled, item)
			}
		}

		// One query for all employees — the N+1 killer.
		employeeIDs := make([]string, 0, len(unsettled))
		for _, item := range unsettled {
			employeeIDs = append(employeeIDs, item.EmployeeID)
		}
		if len(employeeIDs) > 0 {
			if err := tx.Where("id IN ?", employeeIDs).Find(&employees).Error; err != nil {
				return err
			}
		}
		empMap := make(map[string]models.Employee, len(employees))
		for _, emp := range employees {
			empMap[emp.ID] = emp
		}

		// An item whose employee has vanished can never produce a webhook. Counting
		// it in pending_count would strand the batch in processing forever, so it is
		// failed here and excluded from the counter.
		sendable = sendable[:0]
		orphaned := make([]string, 0)
		for _, item := range unsettled {
			if _, ok := empMap[item.EmployeeID]; !ok {
				orphaned = append(orphaned, item.ID)
				continue
			}
			sendable = append(sendable, item)
		}

		// Atomic FSM+counter CAS before the Monnify call — closes the counter race and
		// the duplicate-task race. pending_count counts only items that can still
		// decrement it, so the batch can always reach zero.
		res := tx.Exec(
			`UPDATE payrolls
			    SET status        = ?,
			        pending_count = ?,
			        updated_at    = NOW()
			  WHERE id     = ?
			    AND status IN (?, ?)`,
			models.PayrollProcessing, len(sendable), payrollID,
			models.PayrollPending, models.PayrollFailed,
		)
		if res.Error != nil {
			return fmt.Errorf("payroll %s FSM+counter update failed: %w", payrollID, res.Error)
		}
		if res.RowsAffected == 0 {
			// Already-processing or already-completed — duplicate task. Abort.
			return fmt.Errorf("payroll %s not retryable: %w", payrollID, models.ErrStaleStatus)
		}
		payroll.Status = models.PayrollProcessing
		payroll.PendingCount = len(sendable)

		if len(orphaned) > 0 {
			log.Printf("payroll %s: %d item(s) have no employee record — failing them", payrollID, len(orphaned))
			if err := tx.Model(&models.PayrollItem{}).
				Where("id IN ? AND status = ?", orphaned, models.PayrollPending).
				Updates(map[string]interface{}{
					"status":        models.PayrollFailed,
					"error_message": "employee record not found at disbursement time",
				}).Error; err != nil {
				return fmt.Errorf("payroll %s: failed to mark orphaned items: %w", payrollID, err)
			}
		}
		return nil
	}); err != nil {
		observability.WorkerTasksTotal.WithLabelValues(TypeProcessPayroll, "error").Inc()
		return err
	}

	// Nothing left to disburse — resolve the batch here; no webhook will ever arrive.
	if len(sendable) == 0 {
		log.Printf("payroll %s has no sendable items — reconciling without a Monnify call", payrollID)
		if err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
			return finalizePayroll(tx, orgID, payrollID)
		}); err != nil {
			observability.WorkerTasksTotal.WithLabelValues(TypeProcessPayroll, "error").Inc()
			return err
		}
		observability.WorkerTasksTotal.WithLabelValues(TypeProcessPayroll, "success").Inc()
		return nil
	}

	// Hash map: O(1) employee lookup per item instead of O(N) scans.
	empMap := make(map[string]models.Employee, len(employees))
	for _, emp := range employees {
		empMap[emp.ID] = emp
	}

	// Build the Monnify payload — one line per sendable item, one API call for all of them.
	transactionList := make([]monnify.TransferDetail, 0, len(sendable))
	for _, item := range sendable {
		emp := empMap[item.EmployeeID]
		transactionList = append(transactionList, monnify.TransferDetail{
			// Convert Kobo → Naira exactly once, at the Monnify wire boundary.
			Amount:        item.Amount.Naira(),
			AccountNumber: emp.AccountNumber.String(), // decrypt happens transparently via EncryptedString
			BankCode:      emp.BankCode.String(),
			Narration:     fmt.Sprintf("Salary for %s", payroll.Period),
			Reference:     item.ID,
			CurrencyCode:  "NGN",
		})
	}

	bulkReq := monnify.BulkTransferRequest{
		Title:                     fmt.Sprintf("Payroll Batch %s", payroll.ID),
		BatchReference:            payroll.ID,
		SourceWalletAccountNumber: os.Getenv("MONNIFY_SOURCE_WALLET"),
		TransactionList:           transactionList,
	}

	resp, err := h.MonnifyClient.InitiateBulkTransfer(bulkReq)
	if err != nil {
		// Phase-1 tx is committed; open a fresh RLS scope to mark failed.
		if scopeErr := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
			return models.TransitionStatus(tx, &payroll, models.PayrollProcessing, models.PayrollFailed)
		}); scopeErr != nil {
			log.Printf("payroll %s: failed to mark as failed after Monnify error: %v", payrollID, scopeErr)
		}
		return err
	}
	if !resp.RequestSuccessful {
		if scopeErr := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
			return models.TransitionStatus(tx, &payroll, models.PayrollProcessing, models.PayrollFailed)
		}); scopeErr != nil {
			log.Printf("payroll %s: failed to mark as failed after Monnify rejection: %v", payrollID, scopeErr)
		}
		return fmt.Errorf("monnify said no: %s", resp.ResponseMessage)
	}

	// pending_count was set atomically above — nothing more to do.
	observability.PayrollProcessingDuration.WithLabelValues(orgID).Observe(time.Since(start).Seconds())
	observability.WorkerTasksTotal.WithLabelValues(TypeProcessPayroll, "success").Inc()
	observability.PayrollsCreatedTotal.WithLabelValues(orgID, "processing").Inc()
	log.Printf("Payroll %s handed to Monnify — %d webhooks incoming", payrollID, len(sendable))
	return nil
}

// finalizePayroll resolves a batch that will never receive another webhook: it
// picks completed/failed from the item statuses and CAS-transitions the parent.
// Used when every item is already settled or unsendable.
func finalizePayroll(tx *gorm.DB, orgID, payrollID string) error {
	var payroll models.Payroll
	if err := tx.First(&payroll, "id = ?", payrollID).Error; err != nil {
		return err
	}

	var failedCount int64
	if err := tx.Model(&models.PayrollItem{}).
		Where("payroll_id = ? AND status = ?", payrollID, models.PayrollFailed).
		Count(&failedCount).Error; err != nil {
		return err
	}

	next := models.PayrollCompleted
	if failedCount > 0 {
		next = models.PayrollFailed
	}

	if err := models.TransitionStatus(tx, &payroll, payroll.Status, next); err != nil {
		// A concurrent webhook already resolved the batch — that outcome stands.
		if errors.Is(err, models.ErrStaleStatus) {
			return nil
		}
		return err
	}

	return models.AppendAuditTx(tx, orgID, "Payroll", payrollID, "reconciled",
		string(payroll.Status), string(next), "internal", "")
}
