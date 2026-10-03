package handlers

import (
	"errors"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/services"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgconn"
	"gorm.io/gorm"
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

// GetPayroll — a batch with all its items and the derived outcome.
func (h *PayrollHandler) GetPayroll(c *gin.Context) {
	orgID := middleware.OrgID(c)
	view, err := h.Service.GetPayrollView(c.Request.Context(), orgID, c.Param("id"))
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "payroll not found"})
			return
		}
		middleware.Logger.Error("get payroll failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load payroll"})
		return
	}
	c.JSON(http.StatusOK, view)
}

// ListPayrolls — GET /api/v1/payrolls; the org's payroll history, newest first.
func (h *PayrollHandler) ListPayrolls(c *gin.Context) {
	page, pageSize := pageParams(c)
	orgID := middleware.OrgID(c)
	entries, total, err := h.Service.ListPayrolls(c.Request.Context(), orgID, page, pageSize)
	if err != nil {
		middleware.Logger.Error("list payrolls failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list payrolls"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": entries, "page": page, "page_size": pageSize, "total": total})
}

// RetryPayroll — POST /api/v1/payrolls/:id/retry; admin-only. Sends the failed
// lines of a failed batch to the bank again (or resumes a retry whose job was
// never queued). Paid lines are never touched.
func (h *PayrollHandler) RetryPayroll(c *gin.Context) {
	orgID := middleware.OrgID(c)
	res, err := h.Service.RetryFailedItems(c.Request.Context(), orgID, c.Param("id"),
		services.Actor{Name: middleware.ActorName(c), IP: c.ClientIP()})
	switch {
	case err == nil:
		c.JSON(http.StatusAccepted, res)
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "payroll not found"})
	case errors.Is(err, services.ErrPayrollNotRetryable):
		c.JSON(http.StatusConflict, gin.H{"error": "only a payroll whose payments failed can be retried"})
	case errors.Is(err, services.ErrNothingToRetry):
		skipped := []services.SkippedItem{}
		if res != nil {
			skipped = res.Skipped
		}
		c.JSON(http.StatusConflict, gin.H{
			"error":   "there are no failed payments that can be retried",
			"skipped": skipped,
		})
	case errors.Is(err, services.ErrQueueUnavailable):
		c.Header("Retry-After", "5")
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "the retry was recorded but could not be queued; send the request again"})
	default:
		middleware.Logger.Error("payroll retry failed", "org_id", orgID, "payroll_id", c.Param("id"), "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to retry payroll"})
	}
}

// ResolvePayrollItem — POST /api/v1/payrolls/:id/items/:item_id/resolve;
// admin-only. Settles a payment whose bank callback never arrived, on the
// admin's attestation (reason required, evidence reference optional). Only
// allowed once the callback is overdue.
func (h *PayrollHandler) ResolvePayrollItem(c *gin.Context) {
	var req struct {
		Outcome  string `json:"outcome" binding:"required,oneof=paid failed"`
		Note     string `json:"note" binding:"required"`
		Evidence string `json:"evidence"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBindError(c, err)
		return
	}
	orgID := middleware.OrgID(c)
	item, err := h.Service.ResolveItem(c.Request.Context(), orgID, c.Param("id"), c.Param("item_id"),
		services.ResolveOutcome(req.Outcome), req.Note, req.Evidence,
		services.Actor{Name: middleware.ActorName(c), IP: c.ClientIP()})
	switch {
	case err == nil:
		c.JSON(http.StatusOK, item)
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "payment not found"})
	case errors.Is(err, services.ErrInvalidResolution):
		// Written for people (see ResolveItem); safe to return.
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	case errors.Is(err, services.ErrItemNotResolvable):
		c.JSON(http.StatusConflict, gin.H{"error": "this payment is not awaiting a result"})
	case errors.Is(err, services.ErrNotYetStuck):
		c.JSON(http.StatusConflict, gin.H{"error": "the bank's result is not overdue yet; callbacks normally arrive within minutes"})
	default:
		middleware.Logger.Error("payroll item resolve failed", "org_id", orgID, "item_id", c.Param("item_id"), "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve payment"})
	}
}
