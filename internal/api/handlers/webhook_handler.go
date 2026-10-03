package handlers

import (
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/observability"
	"go-payroll-engine/internal/services"
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

// monnifyEnvelope reads just enough to decide which specific payload shape
// this delivery actually has before committing to one. Monnify's reserved
// account credit notification ("SUCCESSFUL_TRANSACTION") carries a completely
// different eventData shape from a disbursement callback — same envelope,
// same signature scheme, unrelated fields — so eventType has to be read first.
type monnifyEnvelope struct {
	EventType string `json:"eventType"`
}

// HandleMonnifyWebhook — verifies HMAC, then routes by eventType. Disbursement
// callbacks (payroll items, EWA advances) dedupe via bloom and dispatch by the
// reference's ID prefix, as before; a reserved-account credit notification
// (an employer funding their EWA pool) is a structurally different event and
// gets its own path. One endpoint for all of it because Monnify signs every
// callback the same way regardless of event type.
func (h *WebhookHandler) HandleMonnifyWebhook(c *gin.Context) {
	secret := os.Getenv("MONNIFY_SECRET_KEY")
	signature := c.GetHeader("monnify-signature")

	// An unset secret makes HMAC(key="", body) something anyone can compute,
	// so every forged callback would verify. Refuse rather than accept.
	if secret == "" {
		middleware.Logger.Error("monnify webhook rejected: MONNIFY_SECRET_KEY is not set")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "webhook verification is not configured"})
		return
	}

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

	var envelope monnifyEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid payload"})
		return
	}

	if envelope.EventType == "SUCCESSFUL_TRANSACTION" {
		h.handleFundingWebhook(c, body)
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

	// No probabilistic "already seen" short-circuit here, deliberately. A
	// Bloom filter used to sit in front of this and returned 200 on "maybe
	// seen" without touching the database — but a Bloom filter only ever
	// knows "definitely new" vs "maybe seen", and a fixed-size one that
	// never expires saturates toward 100% false positives as references
	// accumulate. Every false positive was a first-time callback Monnify
	// was told had succeeded and then never retried: a payout recorded as
	// neither paid nor failed. The status checks + CAS transitions below
	// are what actually make redelivery idempotent, and they are exact.
	// See TestMonnifyWebhook_FirstDeliveryAlwaysProcessed.

	if strings.HasPrefix(ref, "EWA-") {
		h.handleEWAAdvanceWebhook(c, payload, ref)
		return
	}
	if strings.HasPrefix(ref, "GRANT-") {
		h.handleHardshipGrantWebhook(c, payload, ref)
		return
	}
	h.handlePayrollItemWebhook(c, payload, ref)
}

// handlePayrollItemWebhook processes a disbursement callback for one payroll
// batch item.
func (h *WebhookHandler) handlePayrollItemWebhook(c *gin.Context, payload MonnifyWebhookPayload, ref string) {
	// A retry is sent under "<item id>-R<attempt>" (models.PayrollItem.ItemReference).
	itemID, attempt, ok := models.ParseItemReference(ref)
	if !ok {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	// SECURITY DEFINER lookup (migration 000011) — we don't know the orgID yet, HMAC already vouched for the ref.
	var item models.PayrollItem
	if err := models.DB.Raw(
		"SELECT * FROM lookup_payroll_item_for_webhook(?)", itemID,
	).Scan(&item).Error; err != nil || item.ID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	// A callback for a superseded attempt says nothing about the current one:
	// attempt 1's late "failed" must not fail attempt 2's in-flight payout, and
	// attempt 1's late "paid" must not close an item whose retry is still moving
	// money. Acknowledge it (so the provider stops retrying) and change nothing.
	if attempt != item.Attempt {
		middleware.Logger.Warn("callback for a superseded payout attempt ignored",
			"item_id", item.ID, "callback_attempt", attempt, "current_attempt", item.Attempt)
		c.Status(http.StatusOK)
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
	if err := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
		// Shared with the admin's manual resolution (services.SettlePayrollItem):
		// the item CAS, the pending counter, batch reconciliation and the audit
		// row all commit together, so a failure rolls back and Monnify retries.
		_, err := services.SettlePayrollItem(tx, orgID, &item, newStatus, services.SettlementSource{IP: c.ClientIP()})
		return err
	}); err != nil {
		middleware.Logger.Error("webhook transaction failed",
			"item_id", item.ID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "webhook processing failed"})
		return
	}

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

	c.Status(http.StatusOK)
}

// handleHardshipGrantWebhook resolves a hardship grant's payout: success →
// Disbursed, failure → Failed with the ledger entry reversed. Same shape and
// same lookup-before-scoping justification as handleEWAAdvanceWebhook.
func (h *WebhookHandler) handleHardshipGrantWebhook(c *gin.Context, payload MonnifyWebhookPayload, ref string) {
	// SECURITY DEFINER lookup (migration 000033).
	var grant models.EWAHardshipGrant
	if err := models.DB.Raw(
		"SELECT * FROM lookup_hardship_grant_for_webhook(?)", ref,
	).Scan(&grant).Error; err != nil || grant.ID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Grant not found"})
		return
	}

	// Already resolved — a redelivery. 200 so Monnify stops retrying.
	if grant.Status != models.GrantPending && grant.Status != models.GrantSubmitted {
		c.Status(http.StatusOK)
		return
	}
	orgID := grant.OrganizationID

	var apply func(tx *gorm.DB) error
	switch payload.EventType {
	case "DISBURSEMENT_SUCCESSFUL":
		disbursed, convErr := payload.disbursedKobo()
		if convErr != nil {
			middleware.Logger.Error("grant webhook amount unparseable",
				"grant_id", grant.ID, "raw", payload.EventData.Amount.String(), "error", convErr.Error())
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
			return
		}
		if disbursed != grant.AmountKobo {
			observability.WebhookAmountMismatchTotal.WithLabelValues(orgID).Inc()
			middleware.Logger.Error("CRITICAL: grant webhook amount does not match grant amount",
				"grant_id", grant.ID, "expected_kobo", int64(grant.AmountKobo), "reported_kobo", int64(disbursed))
			if auditErr := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
				return models.AppendAuditTx(tx, orgID, "EWAHardshipGrant", grant.ID, "amount_mismatch",
					grant.AmountKobo.String(), disbursed.String(), c.ClientIP(), "")
			}); auditErr != nil {
				middleware.Logger.Error("grant amount mismatch audit write failed",
					"grant_id", grant.ID, "error", auditErr.Error())
			}
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "amount mismatch"})
			return
		}
		apply = func(tx *gorm.DB) error { return models.ConfirmGrantDisbursed(tx, &grant) }
	case "DISBURSEMENT_FAILED":
		apply = func(tx *gorm.DB) error { return models.FailGrant(tx, &grant, "provider_disbursement_failed") }
	default:
		c.Status(http.StatusOK)
		return
	}

	if err := models.WithOrgScope(c.Request.Context(), orgID, apply); err != nil &&
		!errors.Is(err, models.ErrStaleStatus) { // stale: a concurrent delivery already won
		middleware.Logger.Error("grant webhook processing failed", "grant_id", grant.ID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "webhook processing failed"})
		return
	}
	c.Status(http.StatusOK)
}

// MonnifyFundingWebhookPayload is Monnify's reserved-account credit
// notification. developers.monnify.com was unreachable from the environment
// this was built in (egress-blocked), so this shape follows Monnify's
// long-documented convention rather than a direct read of current docs — see
// the same disclosure on ReserveAccountRequest
// (internal/integrations/monnify/client.go).
type MonnifyFundingWebhookPayload struct {
	EventData struct {
		TransactionReference string      `json:"transactionReference"`
		AmountPaid           json.Number `json:"amountPaid"`
		Product              struct {
			Reference string `json:"reference"`
		} `json:"product"`
	} `json:"eventData"`
}

// handleFundingWebhook credits an employer's EWA funding pool from a deposit
// into their reserved account. Unlike a disbursement callback there is no
// "expected amount" to compare against — any amount deposited is valid — so
// this has no amount-mismatch path.
func (h *WebhookHandler) handleFundingWebhook(c *gin.Context, body []byte) {
	var payload MonnifyFundingWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid payload"})
		return
	}

	ref := payload.EventData.TransactionReference
	accountRef := payload.EventData.Product.Reference
	if ref == "" || accountRef == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Missing transaction or account reference"})
		return
	}

	// SECURITY DEFINER lookup (migration 000019) — same justification as
	// lookup_ewa_advance_for_webhook: HMAC already authenticated the request,
	// and the write below runs inside WithOrgScope using the org_id this
	// lookup reveals.
	var account models.OrganizationFundingAccount
	if err := models.DB.Raw(
		"SELECT * FROM lookup_org_for_funding_account(?)", accountRef,
	).Scan(&account).Error; err != nil || account.OrganizationID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Funding account not found"})
		return
	}

	amountKobo, err := money.FromNairaString(payload.EventData.AmountPaid.String())
	if err != nil {
		middleware.Logger.Error("funding webhook amount unparseable",
			"org_id", account.OrganizationID, "raw", payload.EventData.AmountPaid.String(), "error", err.Error())
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid amount"})
		return
	}

	if err := models.WithOrgScope(c.Request.Context(), account.OrganizationID, func(tx *gorm.DB) error {
		currency, err := models.OrgCurrencyTx(tx, account.OrganizationID)
		if err != nil {
			return err
		}
		return models.RecordEmployerFunding(tx, account.OrganizationID, money.KoboIn(currency, amountKobo), ref)
	}); err != nil {
		middleware.Logger.Error("employer funding deposit failed",
			"org_id", account.OrganizationID, "ref", ref, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "webhook processing failed"})
		return
	}

	c.Status(http.StatusOK)
}
