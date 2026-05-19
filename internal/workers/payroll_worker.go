package workers

import (
	"context"
	"encoding/json"
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

	// Phase 1: load payroll + items, atomically transition FSM + set counter,
	// then load employees — all inside one RLS-scoped transaction. Every query
	// here is automatically filtered to orgID by the Postgres RLS policy, so a
	// rogue task cannot touch another tenant's rows even if payrollID collides.
	var payroll models.Payroll
	var employees []models.Employee

	if err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		if err := tx.Preload("Items").First(&payroll, "id = ?", payrollID).Error; err != nil {
			return err
		}

		// FSM transition AND counter initialization happen as one atomic CAS,
		// strictly BEFORE the Monnify call. This closes two related races:
		//
		//   1. Counter race: if pending_count were set only after Monnify accepts
		//      the batch, webhooks arriving during the Monnify call would
		//      decrement 0 → -1, then the worker's later overwrite to N would
		//      discard those decrements — reconciliation would never fire.
		//   2. Duplicate-task race: two Asynq workers picking up the same job
		//      would both pass the FSM gate if it were a bare UPDATE; the CAS
		//      WHERE status='pending' ensures only one wins.
		// Accept pending OR failed as the source state. failed is the post-retry
		// state — Asynq surfaces a transient worker error by retrying the task,
		// which reloads the payroll from the DB. Without `failed` here, retries
		// would dead-letter forever.
		itemCount := len(payroll.Items)
		res := tx.Exec(
			`UPDATE payrolls
			    SET status        = ?,
			        pending_count = ?,
			        updated_at    = NOW()
			  WHERE id     = ?
			    AND status IN (?, ?)`,
			models.PayrollProcessing, itemCount, payrollID,
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
		payroll.PendingCount = itemCount

		// One query for all employees — the N+1 killer.
		employeeIDs := make([]string, 0, len(payroll.Items))
		for _, item := range payroll.Items {
			employeeIDs = append(employeeIDs, item.EmployeeID)
		}
		return tx.Where("id IN ?", employeeIDs).Find(&employees).Error
	}); err != nil {
		observability.WorkerTasksTotal.WithLabelValues(TypeProcessPayroll, "error").Inc()
		return err
	}

	// Hash map: O(1) employee lookup per item instead of O(N) scans.
	empMap := make(map[string]models.Employee, len(employees))
	for _, emp := range employees {
		empMap[emp.ID] = emp
	}

	// Build the Monnify payload — one line per employee, one API call for all of them.
	var transactionList []monnify.TransferDetail
	for _, item := range payroll.Items {
		emp, ok := empMap[item.EmployeeID]
		if !ok {
			log.Printf("employee %s missing for item %s — skipping", item.EmployeeID, item.ID)
			continue
		}
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
		// Failure transition is its own scoped call — the phase-1 tx already
		// committed, so we open a fresh RLS scope to write the failed status.
		models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error { //nolint:errcheck
			return models.TransitionStatus(tx, &payroll, models.PayrollProcessing, models.PayrollFailed)
		})
		return err
	}
	if !resp.RequestSuccessful {
		models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error { //nolint:errcheck
			return models.TransitionStatus(tx, &payroll, models.PayrollProcessing, models.PayrollFailed)
		})
		return fmt.Errorf("monnify said no: %s", resp.ResponseMessage)
	}

	// pending_count was already set atomically with the FSM transition above —
	// nothing to do here. Webhooks that raced ahead of this point have already
	// decremented from the correct starting value.

	observability.PayrollProcessingDuration.WithLabelValues(orgID).Observe(time.Since(start).Seconds())
	observability.WorkerTasksTotal.WithLabelValues(TypeProcessPayroll, "success").Inc()
	observability.PayrollsCreatedTotal.WithLabelValues(orgID, "processing").Inc()
	log.Printf("Payroll %s handed to Monnify — %d webhooks incoming", payrollID, len(payroll.Items))
	return nil
}
