package workers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"go-payroll-engine/internal/integrations/provider"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/observability"
	"go-payroll-engine/pkg/money"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

// grantTaskMaxRetry — enough to ride out a provider outage; asynq's default
// of 25 would keep hammering a rail for days.
const grantTaskMaxRetry = 10

// EnqueueHardshipGrantDisbursement schedules the payout for a pending grant.
// Called inside IssueHardshipGrant's transaction; TaskID dedupes a repeat
// enqueue of the same grant.
func EnqueueHardshipGrantDisbursement(orgID, grantID string) error {
	payload, err := json.Marshal(map[string]string{"grant_id": grantID, "org_id": orgID})
	if err != nil {
		return err
	}
	_, err = Client.Enqueue(asynq.NewTask(TypeDisburseGrant, payload),
		asynq.TaskID("grant-disburse:"+grantID),
		asynq.MaxRetry(grantTaskMaxRetry),
	)
	return err
}

// HardshipGrantDisbursementHandler pays a pending hardship grant through a
// payment provider. Before it existed, a grant was booked as paid and
// nothing ever moved the money.
type HardshipGrantDisbursementHandler struct {
	Registry *provider.Registry
}

// NewHardshipGrantDisbursementHandler wires up the handler with the given registry.
func NewHardshipGrantDisbursementHandler(registry *provider.Registry) *HardshipGrantDisbursementHandler {
	return &HardshipGrantDisbursementHandler{Registry: registry}
}

var errGrantNotSubmittable = errors.New("hardship grant: no longer pending")

// ProcessHardshipGrantDisbursementTask submits one grant's transfer. Like
// the EWA advance worker it never marks the grant Disbursed — only the
// provider's webhook can say the money landed.
func (h *HardshipGrantDisbursementHandler) ProcessHardshipGrantDisbursementTask(ctx context.Context, t *asynq.Task) error {
	var payload map[string]string
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		return fmt.Errorf("grant task: bad payload: %w: %w", err, asynq.SkipRetry)
	}
	grantID, orgID := payload["grant_id"], payload["org_id"]

	var grant models.EWAHardshipGrant
	var employee models.Employee
	var currency money.Currency
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		// Not found is retried, not skipped: the task is enqueued before
		// IssueHardshipGrant commits, so it can briefly run ahead of the row.
		if err := tx.First(&grant, "id = ?", grantID).Error; err != nil {
			return err
		}
		if grant.Status != models.GrantPending {
			return errGrantNotSubmittable
		}
		var err error
		if currency, err = models.OrgCurrencyTx(tx, orgID); err != nil {
			return err
		}
		return tx.First(&employee, "id = ?", grant.EmployeeID).Error
	})
	if errors.Is(err, errGrantNotSubmittable) {
		return nil
	}
	if err != nil {
		observability.WorkerTasksTotal.WithLabelValues(TypeDisburseGrant, "error").Inc()
		return fmt.Errorf("grant %s: load failed: %w", grantID, err)
	}

	amount := money.KoboIn(currency, grant.AmountKobo)
	pay, err := h.Registry.Select(amount.Currency)
	if err != nil {
		// Configuration, not a transient fault: fail the grant so the ledger
		// is reversed and the admin sees it, rather than leave it pending.
		if failErr := h.fail(ctx, orgID, &grant, "no_provider_for_currency"); failErr != nil {
			return fmt.Errorf("grant %s: %w (and failing it: %w)", grantID, err, failErr)
		}
		observability.WorkerTasksTotal.WithLabelValues(TypeDisburseGrant, "error").Inc()
		return fmt.Errorf("grant %s: %w: %w", grantID, err, asynq.SkipRetry)
	}

	result, err := pay.InitiateTransfer(ctx, provider.TransferRequest{
		Reference:              grant.ID,
		Amount:                 amount,
		RecipientName:          employee.Name,
		RecipientAccountNumber: employee.AccountNumber.String(),
		RecipientBankCode:      employee.BankCode.String(),
		Narration:              "Hardship grant",
	})
	if err != nil {
		// Unreachable or uninterpretable rail: the transfer may or may not
		// exist. Retrying is safe for the same reason it is for EWA advances
		// — every provider here keys InitiateTransfer on our Reference.
		observability.WorkerTasksTotal.WithLabelValues(TypeDisburseGrant, "retry").Inc()
		return fmt.Errorf("grant %s: %w", grantID, err)
	}

	if !result.Accepted {
		// Definite synchronous rejection (bad account, validation): cash
		// never left.
		if err := h.fail(ctx, orgID, &grant, "provider_rejected: "+result.Message); err != nil {
			return fmt.Errorf("grant %s: failing after rejection: %w", grantID, err)
		}
		observability.WorkerTasksTotal.WithLabelValues(TypeDisburseGrant, "success").Inc()
		log.Printf("hardship grant %s rejected by %s: %s", grantID, pay.Name(), result.Message)
		return nil
	}

	err = models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		return models.MarkGrantSubmitted(tx, grant.ID, pay.Name(), result.ProviderReference)
	})
	if errors.Is(err, models.ErrStaleStatus) {
		// The webhook got here first, or a duplicate task already recorded
		// the submission. Either way the grant has moved on.
		return nil
	}
	if err != nil {
		// The provider has the transfer; resubmitting could pay twice. The
		// webhook still resolves the grant by our own ID.
		observability.WorkerTasksTotal.WithLabelValues(TypeDisburseGrant, "error").Inc()
		return fmt.Errorf("grant %s: accepted by %s but failed to record submission: %w: %w",
			grantID, pay.Name(), err, asynq.SkipRetry)
	}

	observability.WorkerTasksTotal.WithLabelValues(TypeDisburseGrant, "success").Inc()
	log.Printf("hardship grant %s submitted to %s — awaiting webhook confirmation", grantID, pay.Name())
	return nil
}

func (h *HardshipGrantDisbursementHandler) fail(ctx context.Context, orgID string, g *models.EWAHardshipGrant, reason string) error {
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		return models.FailGrant(tx, g, reason)
	})
	if errors.Is(err, models.ErrStaleStatus) {
		return nil
	}
	return err
}
