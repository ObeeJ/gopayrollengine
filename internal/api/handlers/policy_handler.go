package handlers

import (
	"errors"
	"net/http"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/pkg/money"

	"github.com/gin-gonic/gin"
)

// PolicyHandler — admin-facing EWA policy configuration. Previously only
// changeable by a direct DB write; every eligibility decision reads the
// saved row, so a change here takes effect on the very next request.
type PolicyHandler struct {
	ewa *services.EWAService
}

// NewPolicyHandler wires up the handler.
func NewPolicyHandler(ewa *services.EWAService) *PolicyHandler {
	return &PolicyHandler{ewa: ewa}
}

// GetPolicy — GET /api/v1/policy. Any employer role may view it — only
// admin may change it (see UpdatePolicy), same read/write split as the
// timesheet review queue.
func (h *PolicyHandler) GetPolicy(c *gin.Context) {
	orgID := middleware.OrgID(c)
	policy, err := h.ewa.GetPolicy(c.Request.Context(), orgID)
	if err != nil {
		middleware.Logger.Error("policy fetch failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load policy"})
		return
	}
	c.JSON(http.StatusOK, policy)
}

// UpdatePolicy — PUT /api/v1/policy. Every field is optional; only the ones
// present are changed. Validated against the same bounds the DB CHECK
// constraints enforce, so a bad value fails with a clear 400 instead of a
// raw constraint violation.
func (h *PolicyHandler) UpdatePolicy(c *gin.Context) {
	var req struct {
		Enabled                *bool       `json:"enabled"`
		MaxAccrualPct          *int        `json:"max_accrual_pct"`
		AbsoluteCapKobo        *money.Kobo `json:"absolute_cap_kobo"`
		MinDrawKobo            *money.Kobo `json:"min_draw_kobo"`
		MaxDrawsPerPeriod      *int        `json:"max_draws_per_period"`
		CoolingOffHours        *int        `json:"cooling_off_hours"`
		EmergencyFloorKobo     *money.Kobo `json:"emergency_floor_kobo"`
		RequireFundingCoverage *bool       `json:"require_funding_coverage"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	orgID := middleware.OrgID(c)
	policy, err := h.ewa.UpdatePolicy(c.Request.Context(), orgID, services.PolicyUpdate{
		Enabled:                req.Enabled,
		MaxAccrualPct:          req.MaxAccrualPct,
		AbsoluteCapKobo:        req.AbsoluteCapKobo,
		MinDrawKobo:            req.MinDrawKobo,
		MaxDrawsPerPeriod:      req.MaxDrawsPerPeriod,
		CoolingOffHours:        req.CoolingOffHours,
		EmergencyFloorKobo:     req.EmergencyFloorKobo,
		RequireFundingCoverage: req.RequireFundingCoverage,
	}, c.ClientIP())
	if err != nil {
		if errors.Is(err, services.ErrInvalidPolicyValue) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		middleware.Logger.Error("policy update failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update policy"})
		return
	}
	c.JSON(http.StatusOK, policy)
}
