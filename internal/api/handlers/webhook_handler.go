package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"
	"io"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type WebhookHandler struct{}

type MonnifyWebhookPayload struct {
	EventType string `json:"eventType"`
	EventData struct {
		BatchReference       string     `json:"batchReference"`
		TransactionReference string     `json:"transactionReference"`
		Status               string     `json:"status"`
		// Monnify sends decimal Naira on the wire; Kobo.UnmarshalJSON parses it.
		Amount money.Kobo `json:"amount"`
	} `json:"eventData"`
}

// HandleMonnifyWebhook — verifies HMAC, dedupes via bloom + DB, transitions the item, reconciles the parent.
func (h *WebhookHandler) HandleMonnifyWebhook(c *gin.Context) {
	secret := os.Getenv("MONNIFY_SECRET_KEY")
	signature := c.GetHeader("monnify-signature")

	// Read body once — used for both HMAC verification and JSON decode.
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		c.Status(http.StatusBadRequest)
		return
	}

	// Step 1: Verify HMAC-SHA512 signature using constant-time comparison.
	mac := hmac.New(sha512.New, []byte(secret))
	mac.Write(body)
	expectedSig := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(signature), []byte(expectedSig)) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Invalid signature"})
		return
	}

	// Step 2: Parse payload before bloom filter so we have the ref key.
	var payload MonnifyWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid payload"})
		return
	}

	ref := payload.EventData.TransactionReference
	if ref == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing transaction reference"})
		return
	}

	// Bloom filter — probable duplicates skip the DB; ~1% false positives fall through.
	if middleware.WebhookBloom != nil {
		ctx := context.Background()
		if seen, err := middleware.WebhookBloom.MightContain(ctx, ref); err == nil && seen {
			c.Status(http.StatusOK)
			return
		}
	}

	// SECURITY DEFINER lookup (migration 000011) — we don't know the orgID yet, HMAC already vouched for the ref.
	var item models.PayrollItem
	if err := models.DB.Raw(
		"SELECT * FROM lookup_payroll_item_for_webhook(?)", ref,
	).Scan(&item).Error; err != nil || item.ID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	// Step 4: DB idempotency — hard guard for bloom filter false positives.
	if item.Status == models.PayrollCompleted || item.Status == models.PayrollFailed {
		c.Status(http.StatusOK)
		return
	}

	// Step 5: Determine new status and validate via FSM.
	var newStatus models.PayrollStatus
	switch payload.EventType {
	case "DISBURSEMENT_SUCCESSFUL":
		newStatus = models.PayrollCompleted
	case "DISBURSEMENT_FAILED":
		newStatus = models.PayrollFailed
	default:
		c.Status(http.StatusOK)
		return
	}

	if !models.CanTransition(item.Status, newStatus) {
		// Log the illegal transition but return 200 so Monnify stops retrying.
		middleware.Logger.Warn("illegal item status transition",
			"item_id", item.ID,
			"from", item.Status,
			"to", newStatus,
		)
		c.Status(http.StatusOK)
		return
	}

	// One RLS-scoped tx for transition + decrement + audit; orgID came from the just-loaded item.
	orgID := item.OrganizationID
	prevStatus := item.Status

	if err := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
		// Step 5 (write): CAS status transition on the item.
		if err := models.TransitionStatus(tx, &item, item.Status, newStatus); err != nil {
			if errors.Is(err, models.ErrStaleStatus) {
				// Concurrent webhook for the same ref already won; treat as success.
				return nil
			}
			return fmt.Errorf("item status update failed: %w", err)
		}

		// UPDATE...RETURNING: exactly one webhook sees pending_count=0, no TOCTOU.
		var post struct {
			PendingCount int                  `gorm:"column:pending_count"`
			Status       models.PayrollStatus `gorm:"column:status"`
		}
		if err := tx.Raw(
			`UPDATE payrolls
			    SET pending_count = pending_count - 1,
			        updated_at    = NOW()
			  WHERE id = ?
			RETURNING pending_count, status`,
			item.PayrollID,
		).Scan(&post).Error; err != nil {
			return fmt.Errorf("atomic pending_count decrement failed: %w", err)
		}
		if post.PendingCount <= 0 {
			reconcilePayrollStatus(tx, orgID, models.Payroll{
				ID:     item.PayrollID,
				Status: post.Status,
			})
		}

		// Audit inside the same tx — failure rolls everything back and Monnify will retry.
		return models.AppendAuditTx(tx, orgID, "PayrollItem", item.ID, "status_change",
			string(prevStatus), string(newStatus), c.ClientIP(), "")
	}); err != nil {
		middleware.Logger.Error("webhook transaction failed",
			"item_id", item.ID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "webhook processing failed"})
		return
	}

	// Mark this ref in the bloom filter so future duplicates skip the DB.
	if middleware.WebhookBloom != nil {
		middleware.WebhookBloom.Add(context.Background(), ref)
	}

	c.Status(http.StatusOK)
}

// reconcilePayrollStatus — called once per batch when pending_count hits zero; CAS UPDATE makes races a no-op.
func reconcilePayrollStatus(tx *gorm.DB, orgID string, payroll models.Payroll) {
	var failedCount int64
	tx.Model(&models.PayrollItem{}).
		Where("payroll_id = ? AND status = ?", payroll.ID, models.PayrollFailed).
		Count(&failedCount)

	newStatus := models.PayrollCompleted
	if failedCount > 0 {
		newStatus = models.PayrollFailed
	}

	// FSM CAS: processing → completed/failed; stale status means someone beat us, also fine.
	if err := models.TransitionStatus(tx, &payroll, payroll.Status, newStatus); err != nil {
		if errors.Is(err, models.ErrStaleStatus) {
			return
		}
		middleware.Logger.Error("payroll reconciliation FSM error",
			"payroll_id", payroll.ID,
			"error", err.Error(),
		)
		return
	}

	// Audit the batch-level resolution inside the same tx.
	if err := models.AppendAuditTx(tx, orgID, "Payroll", payroll.ID, "reconciled",
		string(payroll.Status), string(newStatus), "internal", ""); err != nil {
		middleware.Logger.Error("CRITICAL: reconciliation audit write failed",
			"payroll_id", payroll.ID, "next", newStatus, "error", err.Error())
	}

	middleware.Logger.Info("payroll reconciled",
		"payroll_id", payroll.ID,
		"status", newStatus,
		"failed_items", fmt.Sprintf("%d", failedCount),
	)
}
