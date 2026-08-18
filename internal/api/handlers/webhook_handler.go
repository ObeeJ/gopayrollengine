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
	"go-payroll-engine/internal/observability"
	"go-payroll-engine/pkg/money"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type WebhookHandler struct{}

type MonnifyWebhookPayload struct {
	EventType string `json:"eventType"`
	EventData struct {
		BatchReference       string `json:"batchReference"`
		TransactionReference string `json:"transactionReference"`
		Status               string `json:"status"`
		// Monnify sends decimal *Naira* on the wire ("1500.50"). It is decoded as a
		// raw json.Number and converted through money.FromNairaString so the decimal
		// never touches a float64. Decoding straight into money.Kobo would be a
		// 100× unit error — Kobo.UnmarshalJSON reads a bare number as minor units.
		Amount json.Number `json:"amount"`
	} `json:"eventData"`
}

// disbursedKobo converts the Monnify wire amount (decimal Naira) into Kobo.
func (p MonnifyWebhookPayload) disbursedKobo() (money.Kobo, error) {
	return money.FromNairaString(p.EventData.Amount.String())
}

// HandleMonnifyWebhook — verifies HMAC, dedupes via bloom, then routes to the
// payroll-item or EWA-advance handler by the reference's ID prefix. One
// endpoint for both record types because Monnify's callback shape is
// identical either way — the only thing that differs is which table the
// reference belongs to, and this codebase already names every row with a
// type-tagged prefix ("ITEM-", "EWA-") for exactly this kind of dispatch.
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

	if strings.HasPrefix(ref, "EWA-") {
		h.handleEWAAdvanceWebhook(c, payload, ref)
		return
	}
	h.handlePayrollItemWebhook(c, payload, ref)
}

// handlePayrollItemWebhook processes a disbursement callback for one payroll
// batch item.
func (h *WebhookHandler) handlePayrollItemWebhook(c *gin.Context, payload MonnifyWebhookPayload, ref string) {
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
		// Never mark an item settled without checking that the amount Monnify says
		// it moved equals the amount we asked it to move. A mismatch is either a
		// malformed callback or a disbursement against the wrong item; both need a
		// human, and neither justifies closing the item.
		disbursed, convErr := payload.disbursedKobo()
		if convErr != nil {
			middleware.Logger.Error("webhook amount unparseable",
				"item_id", item.ID, "raw", payload.EventData.Amount.String(), "error", convErr.Error())
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
			return
		}
		if disbursed != item.Amount {
			observability.WebhookAmountMismatchTotal.WithLabelValues(item.OrganizationID).Inc()
			middleware.Logger.Error("CRITICAL: webhook amount does not match item amount",
				"item_id", item.ID,
				"expected_kobo", int64(item.Amount),
				"reported_kobo", int64(disbursed),
			)
			// Record the discrepancy, then refuse. Returning 422 keeps Monnify
			// retrying rather than letting a mismatched settlement pass silently.
			if auditErr := models.WithOrgScope(c.Request.Context(), item.OrganizationID, func(tx *gorm.DB) error {
				return models.AppendAuditTx(tx, item.OrganizationID, "PayrollItem", item.ID, "amount_mismatch",
					item.Amount.String(), disbursed.String(), c.ClientIP(), "")
			}); auditErr != nil {
				middleware.Logger.Error("amount mismatch audit write failed",
					"item_id", item.ID, "error", auditErr.Error())
			}
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "amount mismatch"})
			return
		}
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
		// The pending_count > 0 guard keeps the counter from going negative if a
		// stray webhook ever slips past the guards above; without it the counter
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
			return fmt.Errorf("atomic pending_count decrement failed: %w", res.Error)
		}
		// No row updated means the counter was already zero — the batch has been
		// reconciled by another webhook. The item transition above still stands.
		if res.RowsAffected > 0 && post.PendingCount == 0 {
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

	markSeen(ref)
	c.Status(http.StatusOK)
}

// handleEWAAdvanceWebhook processes a disbursement callback for one EWA
// advance. Mirrors handlePayrollItemWebhook's shape deliberately — same
// lookup-before-scoping problem, same FSM-CAS-then-audit structure — but
// against ewa_advances instead of payroll_items, and with the two outcomes
// mapped onto the advance FSM's own edges (Approved→Disbursed on success,
// Approved→Cancelled, reversing the ledger, on failure) rather than payroll's.
func (h *WebhookHandler) handleEWAAdvanceWebhook(c *gin.Context, payload MonnifyWebhookPayload, ref string) {
	// SECURITY DEFINER lookup (migration 000017) — same justification as
	// lookup_payroll_item_for_webhook: HMAC already authenticated the request,
	// and every write below runs inside WithOrgScope using the org_id this
	// lookup reveals.
	var advance models.EWAAdvance
	if err := models.DB.Raw(
		"SELECT * FROM lookup_ewa_advance_for_webhook(?)", ref,
	).Scan(&advance).Error; err != nil || advance.ID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Advance not found"})
		return
	}

	// DB idempotency — a webhook for an advance already past Approved (settled,
	// cancelled, disbursed by a prior delivery of this same event) is a
	// duplicate. Return 200 so Monnify stops retrying.
	if advance.Status != models.AdvanceApproved {
		c.Status(http.StatusOK)
		return
	}

	orgID := advance.OrganizationID

	switch payload.EventType {
	case "DISBURSEMENT_SUCCESSFUL":
		disbursed, convErr := payload.disbursedKobo()
		if convErr != nil {
			middleware.Logger.Error("ewa webhook amount unparseable",
				"advance_id", advance.ID, "raw", payload.EventData.Amount.String(), "error", convErr.Error())
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
			return
		}
		if disbursed != advance.AmountKobo {
			observability.WebhookAmountMismatchTotal.WithLabelValues(orgID).Inc()
			middleware.Logger.Error("CRITICAL: ewa webhook amount does not match advance amount",
				"advance_id", advance.ID,
				"expected_kobo", int64(advance.AmountKobo),
				"reported_kobo", int64(disbursed),
			)
			if auditErr := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
				return models.AppendAuditTx(tx, orgID, "EWAAdvance", advance.ID, "amount_mismatch",
					advance.AmountKobo.String(), disbursed.String(), c.ClientIP(), "")
			}); auditErr != nil {
				middleware.Logger.Error("ewa amount mismatch audit write failed",
					"advance_id", advance.ID, "error", auditErr.Error())
			}
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "amount mismatch"})
			return
		}

		if err := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
			if err := models.ConfirmDisbursed(tx, &advance); err != nil {
				if errors.Is(err, models.ErrStaleStatus) {
					return nil // a concurrent delivery of this event already won
				}
				return err
			}
			return nil
		}); err != nil {
			middleware.Logger.Error("ewa disbursement confirmation failed",
				"advance_id", advance.ID, "error", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": "webhook processing failed"})
			return
		}

	case "DISBURSEMENT_FAILED":
		// Provider accepted the transfer, then reported it did not complete — cash
		// never actually left, so this reverses exactly like an advance cancelled
		// at settlement time for never having been disbursed at all.
		if err := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
			if err := models.CancelAdvance(tx, &advance, models.AdvanceApproved, "provider_disbursement_failed"); err != nil {
				if errors.Is(err, models.ErrStaleStatus) {
					return nil
				}
				return err
			}
			return nil
		}); err != nil {
			middleware.Logger.Error("ewa disbursement failure handling failed",
				"advance_id", advance.ID, "error", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": "webhook processing failed"})
			return
		}

	default:
		c.Status(http.StatusOK)
		return
	}

	markSeen(ref)
	c.Status(http.StatusOK)
}

// markSeen records ref in the bloom filter so future duplicate deliveries of
// the same event skip straight past the DB lookup.
func markSeen(ref string) {
	if middleware.WebhookBloom == nil {
		return
	}
	if err := middleware.WebhookBloom.Add(context.Background(), ref); err != nil {
		middleware.Logger.Warn("bloom filter add failed", "ref", ref, "error", err.Error())
	}
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
