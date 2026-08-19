package handlers

import (
	"errors"
	"net/http"
	"time"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/services"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// TimeEntryHandler — timesheet endpoints for hourly/gig workers: workers log
// hours, admins approve or reject them.
type TimeEntryHandler struct {
	svc *services.TimeEntryService
}

// NewTimeEntryHandler wires up the handler.
func NewTimeEntryHandler(svc *services.TimeEntryService) *TimeEntryHandler {
	return &TimeEntryHandler{svc: svc}
}

// SubmitTimeEntry — POST /api/v1/worker/time-entries.
func (h *TimeEntryHandler) SubmitTimeEntry(c *gin.Context) {
	var req struct {
		WorkDate      string `json:"work_date" binding:"required"` // "2006-01-02"
		MinutesWorked int    `json:"minutes_worked" binding:"required"`
		Note          string `json:"note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	workDate, err := time.ParseInLocation("2006-01-02", req.WorkDate, time.UTC)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "work_date must be YYYY-MM-DD"})
		return
	}

	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	entry, err := h.svc.SubmitTimeEntry(c.Request.Context(), orgID, employeeID, workDate, req.MinutesWorked, req.Note)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrTimeEntryNotHourly),
			errors.Is(err, services.ErrTimeEntryFutureDate),
			errors.Is(err, services.ErrTimeEntryInvalidRange):
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "employee record not found"})
		default:
			middleware.Logger.Error("time entry submit failed",
				"org_id", orgID, "employee_id", employeeID, "error", err.Error())
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to submit time entry"})
		}
		return
	}
	c.JSON(http.StatusCreated, entry)
}

// GetTimeEntries — GET /api/v1/worker/time-entries; worker's own history.
func (h *TimeEntryHandler) GetTimeEntries(c *gin.Context) {
	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	entries, err := h.svc.ListTimeEntries(c.Request.Context(), orgID, employeeID, "", 100)
	if err != nil {
		middleware.Logger.Error("time entry list failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load time entries"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": entries, "total": len(entries)})
}

// ListPendingTimeEntries — GET /api/v1/time-entries?status=pending; the admin
// review queue. status defaults to "pending"; pass status=all for everything.
func (h *TimeEntryHandler) ListPendingTimeEntries(c *gin.Context) {
	orgID := middleware.OrgID(c)
	status := models.TimeEntryStatus(c.DefaultQuery("status", string(models.TimeEntryPending)))
	if status == "all" {
		status = ""
	}

	entries, err := h.svc.ListTimeEntries(c.Request.Context(), orgID, "", status, 200)
	if err != nil {
		middleware.Logger.Error("time entry admin list failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load time entries"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": entries, "total": len(entries)})
}

// ApproveTimeEntry — POST /api/v1/time-entries/:id/approve.
func (h *TimeEntryHandler) ApproveTimeEntry(c *gin.Context) {
	orgID := middleware.OrgID(c)
	entryID := c.Param("id")

	entry, err := h.svc.ApproveTimeEntry(c.Request.Context(), orgID, entryID, middleware.Role(c), c.ClientIP())
	if err != nil {
		h.handleResolveError(c, orgID, entryID, err)
		return
	}
	c.JSON(http.StatusOK, entry)
}

// RejectTimeEntry — POST /api/v1/time-entries/:id/reject.
func (h *TimeEntryHandler) RejectTimeEntry(c *gin.Context) {
	var req struct {
		Reason string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	orgID := middleware.OrgID(c)
	entryID := c.Param("id")

	entry, err := h.svc.RejectTimeEntry(c.Request.Context(), orgID, entryID, middleware.Role(c), c.ClientIP(), req.Reason)
	if err != nil {
		h.handleResolveError(c, orgID, entryID, err)
		return
	}
	c.JSON(http.StatusOK, entry)
}

func (h *TimeEntryHandler) handleResolveError(c *gin.Context, orgID, entryID string, err error) {
	switch {
	case errors.Is(err, gorm.ErrRecordNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "time entry not found"})
	case errors.Is(err, services.ErrTimeEntryAlreadyResolved):
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
	default:
		middleware.Logger.Error("time entry resolution failed",
			"org_id", orgID, "entry_id", entryID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to resolve time entry"})
	}
}
