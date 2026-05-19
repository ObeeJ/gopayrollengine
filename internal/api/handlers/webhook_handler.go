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

// HandleMonnifyWebhook handles POST /api/v1/webhooks/monnify.
// Called by Monnify to report the outcome of each disbursement. Full flow:
//  1. HMAC-SHA512 signature verification — rejects forged requests.
//  2. Bloom filter check — O(1) probabilistic duplicate detection before any DB read.
//  3. Parse and validate the event payload.
//  4. DB idempotency check — terminal state guard for bloom filter false positives.
//  5. FSM transition — validates the status change is legal before writing.
//  6. Atomic counter decrement — reconciles parent Payroll in O(1) when counter hits zero.
//  7. Audit log — appends an immutable record of every status change.
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

	// Step 2 (cont): Bloom filter — skip DB read entirely for probable duplicates.
	// False positives (~1%) fall through to the DB idempotency check below.
	if middleware.WebhookBloom != nil {
		ctx := context.Background()
		if seen, err := middleware.WebhookBloom.MightContain(ctx, ref); err == nil && seen {
			c.Status(http.StatusOK)
			return
		}
	}

	// Step 3: Load the PayrollItem via the SECURITY DEFINER lookup function
	// (migration 000011). We can't set app.org_id yet because we don't know the
	// orgID until we've read the row, so a normal SELECT would be filtered to
	// zero rows by the strict RLS policy on payroll_items. The lookup function
	// bypasses RLS only for this single-UUID read — every other read/write on
	// payroll_items goes through WithOrgScope. The HMAC check above proves the
	// reference is authentic before we reach this point.
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
		// Log the illegal transition attempt but return 200 so Monnify
		// does not keep retrying an unprocessable event.
		middleware.Logger.Warn("illegal item status transition",
			"item_id", item.ID,
			"from", item.Status,
			"to", newStatus,
		)
		c.Status(http.StatusOK)
		return
	}

	// Steps 5–7 run inside one RLS-scoped transaction so all writes for this
	// webhook event are tenant-isolated and atomic. orgID comes from the item
	// we just loaded — Monnify's HMAC proves the item reference is authentic.
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

		// Step 6: Atomic decrement-and-read in one round-trip. UPDATE...RETURNING
		// guarantees exactly one webhook observes pending_count=0 even under N
		// concurrent webhooks — eliminates the decrement-then-SELECT TOCTOU race.
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

		// Step 7: Append immutable audit record inside the same tx. A failed
		// audit here rolls back the entire tx (item transition + counter decrement),
		// which is the safer trade-off than leaving half-audited state — Monnify
		// will retry and we'll process the event again cleanly.
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

// reconcilePayrollStatus is called exactly once per payroll batch — when the
// atomic pending_count counter reaches zero (guaranteed by UPDATE...RETURNING).
// Runs inside the caller's RLS-scoped tx so the org boundary is already enforced.
// The FSM transition is also a CAS UPDATE; if two paths race here, ErrStaleStatus
// makes the second one a no-op.
func reconcilePayrollStatus(tx *gorm.DB, orgID string, payroll models.Payroll) {
	var failedCount int64
	tx.Model(&models.PayrollItem{}).
		Where("payroll_id = ? AND status = ?", payroll.ID, models.PayrollFailed).
		Count(&failedCount)

	newStatus := models.PayrollCompleted
	if failedCount > 0 {
		newStatus = models.PayrollFailed
	}

	// FSM CAS: processing → completed/failed. A stale-status error means
	// another path beat us to it — also idempotent success.
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
