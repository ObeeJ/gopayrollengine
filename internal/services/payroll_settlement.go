package services

import (
	"errors"
	"fmt"
	"log"
	"time"

	"go-payroll-engine/internal/models"

	"gorm.io/gorm"
)

// SettlementSource says who is settling a payout, for the audit trail.
type SettlementSource struct {
	// IP is the caller's address (the provider's, for a webhook).
	IP string
	// Actor is who settled it by hand; empty for a provider callback.
	Actor string
	// Note and Evidence accompany a manual resolution: what the admin saw and
	// where (e.g. the provider's own transaction reference).
	Note     string
	Evidence string
}

func (s SettlementSource) manual() bool { return s.Actor != "" }

// SettlePayrollItem moves one 'processing' payroll item to its final state,
// decrements the batch's pending counter, and closes the batch when it hits
// zero. It is the single place that does so: the provider's callback and an
// admin's manual resolution must apply exactly the same transitions, or the
// two paths drift and a batch closes differently depending on how it was told.
//
// applied is false when another writer already settled the item (the CAS
// lost); that is success, not an error — callbacks are redelivered.
//
// The caller supplies the org-scoped transaction, so the item transition, the
// counter, the batch reconciliation and the audit row commit together or not
// at all (a failed callback is simply retried by the provider).
func SettlePayrollItem(
	tx *gorm.DB, orgID string, item *models.PayrollItem, next models.PayrollStatus, by SettlementSource,
) (applied bool, err error) {
	prev := item.Status
	if err := models.TransitionStatus(tx, item, item.Status, next); err != nil {
		if errors.Is(err, models.ErrStaleStatus) {
			return false, nil
		}
		return false, fmt.Errorf("item status update failed: %w", err)
	}

	updates := map[string]interface{}{}
	if next == models.PayrollCompleted {
		updates["settled_at"] = time.Now()
	}
	if by.manual() {
		updates["resolved_by"] = by.Actor
		updates["resolution_note"] = by.Note
		if by.Evidence != "" {
			updates["resolution_evidence"] = by.Evidence
		}
	}
	if len(updates) > 0 {
		if err := tx.Model(&models.PayrollItem{}).Where("id = ?", item.ID).Updates(updates).Error; err != nil {
			return false, fmt.Errorf("item settlement stamp failed: %w", err)
		}
	}

	// UPDATE...RETURNING: exactly one settler sees pending_count=0, no TOCTOU.
	var post struct {
		PendingCount int                  `gorm:"column:pending_count"`
		Status       models.PayrollStatus `gorm:"column:status"`
	}
	// The pending_count > 0 guard keeps the counter from going negative if a
	// stray settlement ever slips past the guards above; without it the counter
	// could re-cross zero and reconcile the same batch twice.
	res := tx.Raw(
		`UPDATE payrolls
		    SET pending_count = pending_count - 1,
		        updated_at    = NOW()
		  WHERE id = ?
		    AND pending_count > 0
		RETURNING pending_count, status`,
		item.PayrollID,
	).Scan(&post)
	if res.Error != nil {
		return false, fmt.Errorf("atomic pending_count decrement failed: %w", res.Error)
	}
	// No row updated means the counter was already zero — the batch has been
	// reconciled by another settler. The item transition above still stands.
	if res.RowsAffected > 0 && post.PendingCount == 0 {
		if err := reconcilePayrollStatus(tx, orgID, models.Payroll{ID: item.PayrollID, Status: post.Status}); err != nil {
			return false, err
		}
	}

	action, before, after := "status_change", string(prev), string(next)
	if by.manual() {
		action = "manually_resolved"
		after = string(next) + ": " + by.Note
		if by.Evidence != "" {
			after += " [evidence: " + by.Evidence + "]"
		}
	}
	return true, models.AppendAuditTx(tx, orgID, "PayrollItem", item.ID, action, before, after, by.IP, by.Actor)
}

// reconcilePayrollStatus — called once per batch when pending_count hits zero;
// CAS UPDATE makes races a no-op.
//
// A batch with any failed item must close as 'failed', not 'completed': Monnify
// redelivers. A failed item count used to be ignored, which read as zero
// failures and closed the batch as completed even when some employees'
// transfers had failed.
func reconcilePayrollStatus(tx *gorm.DB, orgID string, payroll models.Payroll) error {
	var failedCount int64
	if err := tx.Model(&models.PayrollItem{}).
		Where("payroll_id = ? AND status = ?", payroll.ID, models.PayrollFailed).
		Count(&failedCount).Error; err != nil {
		return fmt.Errorf("counting failed items for payroll %s: %w", payroll.ID, err)
	}

	newStatus := models.PayrollCompleted
	if failedCount > 0 {
		newStatus = models.PayrollFailed
	}

	// FSM CAS: processing → completed/failed; stale status means someone beat us, also fine.
	if err := models.TransitionStatus(tx, &payroll, payroll.Status, newStatus); err != nil {
		if errors.Is(err, models.ErrStaleStatus) {
			return nil
		}
		return fmt.Errorf("payroll %s reconciliation: %w", payroll.ID, err)
	}

	// Audit the batch-level resolution inside the same tx.
	if err := models.AppendAuditTx(tx, orgID, "Payroll", payroll.ID, "reconciled",
		string(payroll.Status), string(newStatus), "internal", ""); err != nil {
		return fmt.Errorf("payroll %s reconciliation audit: %w", payroll.ID, err)
	}

	log.Printf("payroll reconciled: payroll_id=%s status=%s failed_items=%d", payroll.ID, newStatus, failedCount)
	return nil
}
