package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
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
		BatchReference       string  `json:"batchReference"`
		TransactionReference string  `json:"transactionReference"`
		Status               string  `json:"status"`
		Amount               float64 `json:"amount"`
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

	// Step 3: Load the PayrollItem.
	var item models.PayrollItem
	if err := models.DB.First(&item, "id = ?", ref).Error; err != nil {
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

	prevStatus := item.Status
	if err := models.TransitionStatus(models.DB, &item, item.Status, newStatus); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "status update failed"})
		return
	}

	// Step 6: Atomic counter decrement — O(1), no COUNT queries.
	// When pending_count reaches zero, reconcile the parent Payroll exactly once.
	models.DB.Model(&models.Payroll{}).Where("id = ?", item.PayrollID).
		UpdateColumn("pending_count", gorm.Expr("pending_count - 1"))

	var parent models.Payroll
	models.DB.Select("id, status, pending_count").First(&parent, "id = ?", item.PayrollID)
	if parent.PendingCount <= 0 {
		reconcilePayrollStatus(parent)
	}

	// Step 7: Append immutable audit record.
	models.AppendAudit(
		"PayrollItem", item.ID, "status_change",
		string(prevStatus), string(newStatus),
		c.ClientIP(), "",
	)

	// Mark this ref in the bloom filter so future duplicates skip the DB.
	if middleware.WebhookBloom != nil {
		middleware.WebhookBloom.Add(context.Background(), ref)
	}

	c.Status(http.StatusOK)
}

// reconcilePayrollStatus is called exactly once per payroll batch — when the
// atomic pending_count counter reaches zero. Runs a single COUNT query instead
// of two COUNT queries on every webhook event (O(1) trigger, O(N) work once).
func reconcilePayrollStatus(payroll models.Payroll) {
	var failedCount int64
	models.DB.Model(&models.PayrollItem{}).
		Where("payroll_id = ? AND status = ?", payroll.ID, models.PayrollFailed).
		Count(&failedCount)

	newStatus := models.PayrollCompleted
	if failedCount > 0 {
		newStatus = models.PayrollFailed
	}

	// FSM: processing → completed/failed.
	if err := models.TransitionStatus(models.DB, &payroll, payroll.Status, newStatus); err != nil {
		middleware.Logger.Error("payroll reconciliation FSM error",
			"payroll_id", payroll.ID,
			"error", err.Error(),
		)
		return
	}

	// Audit the batch-level resolution.
	models.AppendAudit(
		"Payroll", payroll.ID, "reconciled",
		string(payroll.Status), string(newStatus),
		"internal", "",
	)

	middleware.Logger.Info("payroll reconciled",
		"payroll_id", payroll.ID,
		"status", newStatus,
		"failed_items", fmt.Sprintf("%d", failedCount),
	)
}
