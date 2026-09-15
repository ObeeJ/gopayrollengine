package handlers

import (
	"errors"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/integrations/banklink"
	"go-payroll-engine/internal/models"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// D2CBankLinkHandler — worker-facing endpoints for linking a bank account
// for read access (payday prediction) and, as a later and separate step,
// authorizing that same account to be debited. Backed by whatever
// banklink.DebitProvider it's constructed with; see routes.go's comment on
// why these routes are only ever registered under MOCK_MODE today — there
// is no real aggregator to point a live user's linking flow at yet.
type D2CBankLinkHandler struct {
	provider banklink.DebitProvider
}

// NewD2CBankLinkHandler — wires up the handler with its provider.
func NewD2CBankLinkHandler(provider banklink.DebitProvider) *D2CBankLinkHandler {
	return &D2CBankLinkHandler{provider: provider}
}

// InitiateLink — POST /api/v1/worker/d2c/bank-link/initiate; starts a link
// flow with the underlying aggregator and hands back whatever the worker's
// client needs to complete it on the aggregator's own UI.
func (h *D2CBankLinkHandler) InitiateLink(c *gin.Context) {
	employeeID := middleware.EmployeeID(c)

	session, err := h.provider.InitiateLink(c.Request.Context(), employeeID)
	if err != nil {
		middleware.Logger.Error("d2c bank-link initiate failed", "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "failed to start bank linking"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"session_token": session.SessionToken,
		"redirect_url":  session.RedirectURL,
	})
}

// CompleteLink — POST /api/v1/worker/d2c/bank-link/complete; exchanges the
// aggregator's callback token for a durable account reference, persists the
// link, and records the worker's consent to read from it. This consent is
// deliberately separate from D2C signup's own disclosure consent — no
// account had been chosen yet at signup time — and from the later debit
// authorization consent below; see migration 000030/000031's comments on
// why linking and debit authorization must never share one consent.
func (h *D2CBankLinkHandler) CompleteLink(c *gin.Context) {
	var req struct {
		CallbackToken string `json:"callback_token" binding:"required"`
		Consent       bool   `json:"consent" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	orgID := middleware.OrgID(c)
	employeeID := middleware.EmployeeID(c)

	account, err := h.provider.CompleteLink(c.Request.Context(), req.CallbackToken)
	if err != nil {
		middleware.Logger.Error("d2c bank-link complete failed", "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to complete bank linking"})
		return
	}

	var link models.D2CBankLink
	expires := time.Now().AddDate(1, 0, 0)
	err = models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
		link = models.D2CBankLink{
			OrganizationID:     orgID,
			EmployeeID:         employeeID,
			Provider:           h.provider.Name(),
			ProviderAccountRef: account.ProviderAccountRef,
			Status:             models.D2CBankLinkLinked,
			LinkedAt:           time.Now(),
		}
		if err := tx.Create(&link).Error; err != nil {
			return err
		}
		return tx.Create(&models.ConsentRecord{
			OrganizationID: orgID,
			EmployeeID:     employeeID,
			ConsentType:    "d2c_bank_link_read",
			Granted:        true,
			IPAddress:      c.ClientIP(),
			UserAgent:      c.Request.UserAgent(),
			ConsentedAt:    time.Now(),
			ExpiresAt:      &expires,
		}).Error
	})
	if err != nil {
		middleware.Logger.Error("d2c bank-link persist failed", "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save linked account"})
		return
	}

	c.JSON(http.StatusCreated, link)
}

// AuthorizeDebit — POST /api/v1/worker/d2c/bank-link/authorize-debit; the
// second, later, separate consent — standing authorization to debit the
// worker's already-linked account on a predicted payday. Requires a link in
// D2CBankLinkLinked status; there is nothing to authorize a debit against
// otherwise.
func (h *D2CBankLinkHandler) AuthorizeDebit(c *gin.Context) {
	var req struct {
		Consent bool `json:"consent" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	orgID := middleware.OrgID(c)
	employeeID := middleware.EmployeeID(c)

	var link models.D2CBankLink
	if err := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
		return tx.Where("employee_id = ? AND status = ?", employeeID, models.D2CBankLinkLinked).First(&link).Error
	}); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "no linked bank account found — link one first"})
			return
		}
		middleware.Logger.Error("d2c debit-mandate lookup failed", "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to authorize debit"})
		return
	}

	mandateRef, err := h.provider.AuthorizeDebitMandate(c.Request.Context(), link.ProviderAccountRef)
	if err != nil {
		middleware.Logger.Error("d2c debit-mandate authorize failed", "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusBadGateway, gin.H{"error": "failed to authorize debit"})
		return
	}

	now := time.Now()
	expires := now.AddDate(1, 0, 0)
	err = models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
		if err := tx.Model(&link).Updates(map[string]interface{}{
			"debit_mandate_ref":   mandateRef,
			"debit_authorized_at": now,
		}).Error; err != nil {
			return err
		}
		return tx.Create(&models.ConsentRecord{
			OrganizationID: orgID,
			EmployeeID:     employeeID,
			ConsentType:    "d2c_debit_mandate",
			Granted:        true,
			IPAddress:      c.ClientIP(),
			UserAgent:      c.Request.UserAgent(),
			ConsentedAt:    now,
			ExpiresAt:      &expires,
		}).Error
	})
	if err != nil {
		middleware.Logger.Error("d2c debit-mandate persist failed", "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to save debit authorization"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"debit_mandate_ref":   mandateRef,
		"debit_authorized_at": now,
	})
}
