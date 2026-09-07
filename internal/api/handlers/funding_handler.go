package handlers

import (
	"errors"
	"net/http"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/services"

	"github.com/gin-gonic/gin"
)

// FundingHandler — employer-facing endpoints for the org's EWA funding pool:
// the dedicated deposit account and how much has been drawn against it.
type FundingHandler struct {
	svc *services.FundingService
}

// NewFundingHandler wires up the handler.
func NewFundingHandler(svc *services.FundingService) *FundingHandler {
	return &FundingHandler{svc: svc}
}

// ProvisionFundingAccount — POST /api/v1/funding-account. Idempotent: returns
// the existing account if the org already has one.
func (h *FundingHandler) ProvisionFundingAccount(c *gin.Context) {
	var req struct {
		ContactEmail string `json:"contact_email" binding:"required,email"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	orgID := middleware.OrgID(c)
	account, err := h.svc.ProvisionAccount(c.Request.Context(), orgID, req.ContactEmail)
	if err != nil {
		if errors.Is(err, services.ErrFundingAccountProviderRejected) {
			c.JSON(http.StatusBadGateway, gin.H{"error": err.Error()})
			return
		}
		middleware.Logger.Error("funding account provisioning failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to provision funding account"})
		return
	}
	c.JSON(http.StatusOK, account)
}

// GetFundingStatus — GET /api/v1/funding-account. Returns the account (if
// provisioned) and current exposure — see models.FundingExposure.
func (h *FundingHandler) GetFundingStatus(c *gin.Context) {
	orgID := middleware.OrgID(c)
	status, err := h.svc.GetFundingStatus(c.Request.Context(), orgID)
	if err != nil {
		middleware.Logger.Error("funding status lookup failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load funding status"})
		return
	}
	c.JSON(http.StatusOK, status)
}
