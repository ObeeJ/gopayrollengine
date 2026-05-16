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

	var payroll models.Payroll
	// Scoped to org — a rogue task cannot process another tenant's payroll.
	if err := models.ScopedDB(orgID).Preload("Items").First(&payroll, "id = ?", payrollID).Error; err != nil {
		return err
	}

	// FSM gate: pending → processing or bust.
	if err := models.TransitionStatus(models.DB, &payroll, payroll.Status, models.PayrollProcessing); err != nil {
		return fmt.Errorf("payroll %s FSM rejected transition: %w", payrollID, err)
	}

	// One query for all employees — the N+1 killer.
	employeeIDs := make([]string, 0, len(payroll.Items))
	for _, item := range payroll.Items {
		employeeIDs = append(employeeIDs, item.EmployeeID)
	}

	var employees []models.Employee
	if err := models.ScopedDB(orgID).Where("id IN ?", employeeIDs).Find(&employees).Error; err != nil {
		models.TransitionStatus(models.DB, &payroll, models.PayrollProcessing, models.PayrollFailed)
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
			Amount:        item.Amount,
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
		models.TransitionStatus(models.DB, &payroll, models.PayrollProcessing, models.PayrollFailed)
		return err
	}
	if !resp.RequestSuccessful {
		models.TransitionStatus(models.DB, &payroll, models.PayrollProcessing, models.PayrollFailed)
		return fmt.Errorf("monnify said no: %s", resp.ResponseMessage)
	}

	// Monnify accepted the batch — set the counter so webhooks can reconcile atomically.
	models.DB.Model(&payroll).UpdateColumn("pending_count", len(payroll.Items))

	observability.PayrollProcessingDuration.WithLabelValues(orgID).Observe(time.Since(start).Seconds())
	observability.WorkerTasksTotal.WithLabelValues(TypeProcessPayroll, "success").Inc()
	observability.PayrollsCreatedTotal.WithLabelValues(orgID, "processing").Inc()
	log.Printf("Payroll %s handed to Monnify — %d webhooks incoming", payrollID, len(payroll.Items))
	return nil
}
