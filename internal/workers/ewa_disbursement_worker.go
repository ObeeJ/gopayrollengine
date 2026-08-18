package workers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"go-payroll-engine/internal/integrations/provider"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/observability"
	"go-payroll-engine/pkg/money"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

// EWADisbursementHandler submits an approved advance to a payment provider.
// This is the piece that was missing before: RequestAdvance posted a ledger
// entry and stopped, so an "approved" advance was money the ledger claimed had
// moved while nothing had actually asked a bank to move it.
type EWADisbursementHandler struct {
	Registry *provider.Registry
}

// NewEWADisbursementHandler wires up the handler with the given registry.
func NewEWADisbursementHandler(registry *provider.Registry) *EWADisbursementHandler {
	return &EWADisbursementHandler{Registry: registry}
}

// errNotSubmittable means the advance is no longer in a state this task
// should act on — already submitted, or moved past Approved by something else
// (a settlement pass that cancelled it, most likely). Not a failure: whatever
// changed its state already recorded why. Modelled as a sentinel rather than
// re-deriving the same "is this still submittable" check twice, once inside
// the transaction and again outside it.
var errNotSubmittable = errors.New("ewa disbursement: advance is no longer submittable")

// ProcessEWADisbursementTask submits one advance's transfer and records the
// provider's response. It does NOT transition the advance to Disbursed —
// acceptance by a provider means the rail queued the transfer, not that money
// landed. That transition happens only when the provider's webhook confirms
// it, in HandleDisbursementWebhook. Treating "accepted" as "disbursed" would
// let a transfer the bank later rejects sit in the ledger as settled money
// that never moved.
func (h *EWADisbursementHandler) ProcessEWADisbursementTask(ctx context.Context, t *asynq.Task) error {
	start := time.Now()
	var payload map[string]string
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		observability.WorkerTasksTotal.WithLabelValues(TypeDisburseEWAAdvance, "error").Inc()
		return err
	}

	advanceID := payload["advance_id"]
	orgID := payload["org_id"]
	log.Printf("Submitting EWA advance %s for org %s", advanceID, orgID)

	var advance models.EWAAdvance
	var employee models.Employee

	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		if err := tx.First(&advance, "id = ?", advanceID).Error; err != nil {
			return err
		}
		if advance.ProviderReference != nil || advance.Status != models.AdvanceApproved {
			return errNotSubmittable
		}
		return tx.First(&employee, "id = ?", advance.EmployeeID).Error
	})
	if errors.Is(err, errNotSubmittable) {
		observability.WorkerTasksTotal.WithLabelValues(TypeDisburseEWAAdvance, "success").Inc()
		return nil
	}
	if err != nil {
		observability.WorkerTasksTotal.WithLabelValues(TypeDisburseEWAAdvance, "error").Inc()
		return fmt.Errorf("ewa advance %s: load failed: %w", advanceID, err)
	}

	amount := money.NGNFromKobo(advance.AmountKobo)
	pay, err := h.Registry.Select(amount.Currency)
	if err != nil {
		// No provider can settle this currency at all — a configuration problem,
		// not a transient one. Retrying will not fix it, so fail loudly enough
		// to page rather than let Asynq retry forever.
		observability.WorkerTasksTotal.WithLabelValues(TypeDisburseEWAAdvance, "error").Inc()
		return fmt.Errorf("ewa advance %s: %w", advanceID, err)
	}

	result, err := pay.InitiateTransfer(ctx, provider.TransferRequest{
		Reference:              advance.ID,
		Amount:                 amount,
		RecipientName:          employee.Name,
		RecipientAccountNumber: employee.AccountNumber.String(),
		RecipientBankCode:      employee.BankCode.String(),
		Narration:              fmt.Sprintf("Earned wage access — %s", advance.Period),
	})
	if err != nil {
		if errors.Is(err, provider.ErrProviderUnavailable) {
			// The rail could not be reached, or gave an answer we could not
			// interpret. Crucially: the transfer may still have gone through on
			// the provider's side despite the failure to confirm it — returning
			// an error here lets Asynq retry, and the retry is safe because
			// InitiateTransfer is keyed by our Reference, which every provider in
			// this codebase treats as an idempotency key.
			observability.WorkerTasksTotal.WithLabelValues(TypeDisburseEWAAdvance, "retry").Inc()
			observability.EWADisbursementOutcomesTotal.WithLabelValues(pay.Name(), "provider_unavailable").Inc()
			return err
		}
		observability.WorkerTasksTotal.WithLabelValues(TypeDisburseEWAAdvance, "error").Inc()
		return fmt.Errorf("ewa advance %s: %w", advanceID, err)
	}

	if !result.Accepted {
		// A definite, synchronous rejection — bad account number, provider-side
		// validation failure. Cash never left, so this reverses exactly like an
		// advance that was never disbursed at settlement time; see
		// models.CancelAdvance for why the two share one code path.
		cancelErr := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
			return models.CancelAdvance(tx, &advance, models.AdvanceApproved,
				fmt.Sprintf("provider_rejected: %s", result.Message))
		})
		if cancelErr != nil && !errors.Is(cancelErr, models.ErrStaleStatus) {
			observability.WorkerTasksTotal.WithLabelValues(TypeDisburseEWAAdvance, "error").Inc()
			return fmt.Errorf("ewa advance %s: cancellation after rejection failed: %w", advanceID, cancelErr)
		}
		observability.WorkerTasksTotal.WithLabelValues(TypeDisburseEWAAdvance, "success").Inc()
		observability.EWADisbursementOutcomesTotal.WithLabelValues(pay.Name(), "rejected").Inc()
		log.Printf("EWA advance %s rejected by %s: %s", advanceID, pay.Name(), result.Message)
		return nil
	}

	if err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		return models.MarkSubmittedToProvider(tx, advance.ID, pay.Name(), result.ProviderReference)
	}); err != nil {
		// The provider has this transfer moving regardless of whether we managed
		// to record that fact locally, so this must NOT be retried by resubmitting
		// (that would risk a second real transfer). It is, however, an
		// operational gap worth a human: the webhook handler looks up advances by
		// our own ID, not the provider's reference, so confirmation still arrives
		// — but nothing here recorded which provider or reference to reconcile
		// against if that webhook is ever late or lost.
		observability.WorkerTasksTotal.WithLabelValues(TypeDisburseEWAAdvance, "error").Inc()
		return fmt.Errorf("ewa advance %s: accepted by %s but failed to record submission: %w",
			advanceID, pay.Name(), err)
	}

	observability.EWADisbursementDuration.WithLabelValues(pay.Name()).Observe(time.Since(start).Seconds())
	observability.WorkerTasksTotal.WithLabelValues(TypeDisburseEWAAdvance, "success").Inc()
	observability.EWADisbursementOutcomesTotal.WithLabelValues(pay.Name(), "submitted").Inc()
	log.Printf("EWA advance %s submitted to %s — awaiting webhook confirmation", advanceID, pay.Name())
	return nil
}
