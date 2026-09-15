package handlers

import (
	"errors"
	"net/http"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/services"

	"github.com/gin-gonic/gin"
)

// PayrollPolicyHandler — admin-facing shift-differential/overtime
// configuration. Mirrors PolicyHandler (EWA policy): every hourly payroll
// run reads the saved row, so a change here takes effect on the very next
// run.
type PayrollPolicyHandler struct {
	payroll *services.PayrollService
}

// NewPayrollPolicyHandler wires up the handler.
func NewPayrollPolicyHandler(payroll *services.PayrollService) *PayrollPolicyHandler {
	return &PayrollPolicyHandler{payroll: payroll}
}

// GetPayrollPolicy — GET /api/v1/payroll-policy. Any employer role may view
// it — only admin may change it, same read/write split as the EWA policy.
func (h *PayrollPolicyHandler) GetPayrollPolicy(c *gin.Context) {
	orgID := middleware.OrgID(c)
	policy, err := h.payroll.GetPayrollPolicy(c.Request.Context(), orgID)
	if err != nil {
		middleware.Logger.Error("payroll policy fetch failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load payroll policy"})
		return
	}
	c.JSON(http.StatusOK, policy)
}

// UpdatePayrollPolicy — PUT /api/v1/payroll-policy. Every field is optional;
// only the ones present are changed.
func (h *PayrollPolicyHandler) UpdatePayrollPolicy(c *gin.Context) {
	var req struct {
		OvertimeThresholdMinutesPerWeek *int `json:"overtime_threshold_minutes_per_week"`
		OvertimeMultiplierBps           *int `json:"overtime_multiplier_bps"`
		NightShiftMultiplierBps         *int `json:"night_shift_multiplier_bps"`
		WeekendShiftMultiplierBps       *int `json:"weekend_shift_multiplier_bps"`
		HolidayShiftMultiplierBps       *int `json:"holiday_shift_multiplier_bps"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	orgID := middleware.OrgID(c)
	policy, err := h.payroll.UpdatePayrollPolicy(c.Request.Context(), orgID, services.PayrollPolicyUpdate{
		OvertimeThresholdMinutesPerWeek: req.OvertimeThresholdMinutesPerWeek,
		OvertimeMultiplierBps:           req.OvertimeMultiplierBps,
		NightShiftMultiplierBps:         req.NightShiftMultiplierBps,
		WeekendShiftMultiplierBps:       req.WeekendShiftMultiplierBps,
		HolidayShiftMultiplierBps:       req.HolidayShiftMultiplierBps,
	}, c.ClientIP())
	if err != nil {
		if errors.Is(err, services.ErrInvalidPolicyValue) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		middleware.Logger.Error("payroll policy update failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update payroll policy"})
		return
	}
	c.JSON(http.StatusOK, policy)
}
