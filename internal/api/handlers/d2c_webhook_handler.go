package handlers

import (
	"errors"
	"net/http"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// D2CDebitWebhookPayload is the shape this handler expects from a debit
// provider confirming a collection attempt's outcome. No real aggregator is
// wired yet (see internal/integrations/banklink's package doc) — this is a
// placeholder shape, not a documented Mono/Okra webhook format. Whichever
// provider is actually integrated will very likely need its own payload
// type and its own signature verification here, the same way
// HandleMonnifyWebhook's HMAC-SHA512 check is Monnify-specific.
type D2CDebitWebhookPayload struct {
	ProviderReference string `json:"provider_reference" binding:"required"`
	Status            string `json:"status" binding:"required"` // "successful" | "failed"
	Reason            string `json:"reason"`
}

// HandleD2CDebitWebhook — POST /api/v1/webhooks/d2c-debit-collection.
// Confirms a pending D2CDebitCollection's real outcome — see
// models.ConfirmD2CCollectionSuccess/Failure for what happens next.
func (h *WebhookHandler) HandleD2CDebitWebhook(c *gin.Context) {
	var payload D2CDebitWebhookPayload
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	// SECURITY DEFINER lookup (migration 000031) — same justification as
	// lookup_payroll_item_for_webhook and lookup_ewa_advance_for_webhook: the
	// callback doesn't carry our org_id, so a single-row unscoped read is how
	// this handler learns it before opening a properly org-scoped
	// transaction for everything else.
	var collection models.D2CDebitCollection
	if err := models.DB.Raw(
		"SELECT * FROM lookup_d2c_collection_for_webhook(?)", payload.ProviderReference,
	).Scan(&collection).Error; err != nil || collection.ID == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "collection attempt not found"})
		return
	}

	// DB idempotency — a webhook for a collection already resolved (a
	// concurrent delivery of the same event) is a duplicate. Return 200 so
	// the provider stops retrying, the same posture every other webhook in
	// this codebase takes.
	if collection.Status != models.D2CCollectionPending {
		c.Status(http.StatusOK)
		return
	}

	orgID := collection.OrganizationID

	var confirmErr error
	switch payload.Status {
	case "successful":
		confirmErr = models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
			return models.ConfirmD2CCollectionSuccess(tx, &collection)
		})
	case "failed":
		confirmErr = models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
			return models.ConfirmD2CCollectionFailure(tx, &collection, payload.Reason)
		})
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "status must be 'successful' or 'failed'"})
		return
	}
	if confirmErr != nil {
		if errors.Is(confirmErr, models.ErrD2CCollectionAlreadyResolved) {
			c.Status(http.StatusOK)
			return
		}
		middleware.Logger.Error("d2c debit collection confirmation failed",
			"collection_id", collection.ID, "org_id", orgID, "error", confirmErr.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "webhook processing failed"})
		return
	}

	c.Status(http.StatusOK)
}
