package handlers

import (
	"errors"
	"net/http"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/pkg/money"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// AdvanceHandler — worker-facing EWA endpoints, all scoped through RLS.
type AdvanceHandler struct {
	ewa *services.EWAService
}

// NewAdvanceHandler — wires up the handler.
func NewAdvanceHandler(ewa *services.EWAService) *AdvanceHandler {
	return &AdvanceHandler{ewa: ewa}
}

// GetEarnedWages — GET /api/v1/worker/wages.
//
// Returns the full basis for the number, not just the number: what has accrued,
// what the policy allows, what dependency tier the worker is in and why. A cap
// a worker cannot interrogate is a cap they cannot plan around.
func (h *AdvanceHandler) GetEarnedWages(c *gin.Context) {
	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	el, err := h.ewa.GetEligibility(c.Request.Context(), orgID, employeeID, time.Now())
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "employee record not found"})
			return
		}
		middleware.Logger.Error("eligibility computation failed",
			"org_id", orgID, "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load earned wages"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"employee_id":      el.EmployeeID,
		"period":           el.Period,
		"monthly_salary":   el.MonthlySalary,
		"earned_to_date":   el.AccruedToDate,
		"max_advance":      el.Available,
		"policy_cap":       el.PolicyCap,
		"tier_cap":         el.TierCap,
		"outstanding":      el.Outstanding,
		"minimum_draw":     el.MinimumDraw,
		"draws_this_month": el.DrawsThisPeriod,
		"max_draws":        el.MaxDraws,
		"next_eligible_at": el.NextEligibleAt,
		"blocked":          el.Blocked,
		"blocked_reason":   el.BlockedReason,
		// The consequence next to the offer, not a surprise on payday: UK user
		// research finds people grasp "money available now" but are caught off
		// guard by a smaller paycheck. protected_payday is the worker's own floor
		// (0 if unset) — the one guardrail that isn't the system's judgement call.
		"projected_payday":              el.ProjectedPayday,
		"projected_payday_if_max_drawn": el.ProjectedPaydayIfMaxDrawn,
		"protected_payday":              el.ProtectedPayday,
		"wellbeing": gin.H{
			"tier":    el.Dependency.Tier,
			"score":   el.Dependency.Score,
			"signals": el.Dependency.Signals,
			"notes":   el.Dependency.Reasons,
		},
	})
}

// RequestAdvance — POST /api/v1/worker/advances.
//
// The amount is the only thing the client controls. Every limit is recomputed
// server-side inside the writing transaction, so a client cannot supply its own
// cap and a concurrent request cannot race past one.
func (h *AdvanceHandler) RequestAdvance(c *gin.Context) {
	var req struct {
		Amount money.Kobo `json:"amount" binding:"required"`
		Reason string     `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !req.Amount.IsPositive() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "amount must be positive"})
		return
	}

	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	advance, el, err := h.ewa.RequestAdvance(
		c.Request.Context(), orgID, employeeID, req.Amount,
		c.GetHeader("Idempotency-Key"), c.ClientIP(),
	)

	// A declined request is a recorded, explained decision — not a server error.
	if errors.Is(err, services.ErrAdvanceDeclined) {
		body := gin.H{
			"status":         string(models.AdvanceDeclined),
			"decline_reason": advance.DeclineReason,
			"advance_id":     advance.ID,
		}
		if el != nil {
			body["available"] = el.Available
			body["minimum_draw"] = el.MinimumDraw
			body["next_eligible_at"] = el.NextEligibleAt
			body["wellbeing"] = gin.H{
				"tier":  el.Dependency.Tier,
				"notes": el.Dependency.Reasons,
			}
		}
		c.JSON(http.StatusUnprocessableEntity, body)
		return
	}
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "employee record not found"})
			return
		}
		middleware.Logger.Error("advance request failed",
			"org_id", orgID, "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create advance request"})
		return
	}

	c.JSON(http.StatusAccepted, advance)
}

// SetProtectedPayday — POST /api/v1/worker/protected-payday.
//
// Lets a worker set "protect this much of my next payday" — the one guardrail
// that is the worker's own choice rather than the system's judgement call.
// Raising it is immediate; lowering it is rate-limited by the org's cooling-off
// policy so it cannot be dropped in the moment of temptation.
func (h *AdvanceHandler) SetProtectedPayday(c *gin.Context) {
	var req struct {
		Amount money.Kobo `json:"amount" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.Amount.IsNegative() {
		c.JSON(http.StatusBadRequest, gin.H{"error": "amount must not be negative"})
		return
	}

	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	pref, err := h.ewa.SetProtectedPayday(c.Request.Context(), orgID, employeeID, req.Amount, time.Now())
	if err != nil {
		if errors.Is(err, services.ErrFloorLoweringRateLimited) {
			c.JSON(http.StatusUnprocessableEntity, gin.H{"error": err.Error()})
			return
		}
		middleware.Logger.Error("set protected payday failed",
			"org_id", orgID, "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update protected payday"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"protected_payday": pref.ProtectedPayday(),
		"last_changed_at":  pref.LastChangedAt,
	})
}

// GetAdvanceHistory — GET /api/v1/worker/advances; worker's own history, RLS-fenced.
func (h *AdvanceHandler) GetAdvanceHistory(c *gin.Context) {
	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	var advances []models.EWAAdvance
	if err := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
		return tx.Where("employee_id = ?", employeeID).
			Order("requested_at desc").
			Limit(100).
			Find(&advances).Error
	}); err != nil {
		middleware.Logger.Error("advance history fetch failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load advance history"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": advances, "total": len(advances)})
}
