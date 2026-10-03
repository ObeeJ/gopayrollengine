package handlers

import (
	"errors"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/services"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
)

type PayrollHandler struct {
	Service *services.PayrollService
}

// CreatePayroll — admin-only; returns 202 because the worker actually moves the money.
func (h *PayrollHandler) CreatePayroll(c *gin.Context) {
	var req struct {
		Period string `json:"period" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBindError(c, err)
		return
	}

	orgID := middleware.OrgID(c)
	payroll, err := h.Service.CreatePayroll(c.Request.Context(), orgID, req.Period)
	if err != nil {
		var pgErr *pgconn.PgError
		switch {
		case errors.Is(err, services.ErrInvalidPeriod):
			c.JSON(http.StatusBadRequest, gin.H{"error": "period must be YYYY-MM, e.g. 2026-09"})
		case errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation:
			// idx_payrolls_period_org — this month has already been run.
			c.JSON(http.StatusConflict, gin.H{"error": "a payroll for this period already exists"})
		case errors.Is(err, services.ErrNoActiveEmployees):
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "no active employees to pay"})
		case errors.Is(err, services.ErrUnsupportedCurrency):
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": "no payment provider can settle this organization's currency"})
		default:
			// Never echo err itself: it can carry SQL and internal identifiers.
			middleware.Logger.Error("create payroll failed", "org_id", orgID, "error", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create payroll"})
		}
		return
	}
	c.JSON(http.StatusAccepted, payroll)
}

// GetPayroll — loads the batch and all its items via the service layer.
func (h *PayrollHandler) GetPayroll(c *gin.Context) {
	id := c.Param("id")
	payroll, err := h.Service.GetPayroll(c.Request.Context(), middleware.OrgID(c), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "payroll not found"})
		return
	}
	c.JSON(http.StatusOK, payroll)
}
