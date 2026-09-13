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
			"nudge":   el.Dependency.Nudge,
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
				"nudge": el.Dependency.Nudge,
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

// SetSavingsPreference — POST /api/v1/worker/savings.
//
// Lets a worker opt into automated savings: a fixed share of net pay, or the
// round-up "spare change" above a chosen unit, diverted every payroll run.
// Opt-in and worker-controlled, exactly like the protected-payday floor.
func (h *AdvanceHandler) SetSavingsPreference(c *gin.Context) {
	var req struct {
		Enabled bool               `json:"enabled"`
		Mode    models.SavingsMode `json:"mode"`
		Amount  int64              `json:"amount"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	pref, err := h.ewa.SetSavingsPreference(c.Request.Context(), orgID, employeeID, services.SavingsPreferenceUpdate{
		Enabled: req.Enabled, Mode: req.Mode, Amount: req.Amount,
	})
	if err != nil {
		if errors.Is(err, services.ErrInvalidSavingsPreference) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		middleware.Logger.Error("set savings preference failed",
			"org_id", orgID, "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update savings preference"})
		return
	}

	c.JSON(http.StatusOK, pref)
}

// GetSavings — GET /api/v1/worker/savings; the worker's current election
// plus their running savings balance.
func (h *AdvanceHandler) GetSavings(c *gin.Context) {
	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	pref, err := h.ewa.GetSavingsPreference(c.Request.Context(), orgID, employeeID)
	if err != nil {
		middleware.Logger.Error("get savings preference failed",
			"org_id", orgID, "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load savings preference"})
		return
	}
	balance, err := h.ewa.GetSavingsBalance(c.Request.Context(), orgID, employeeID)
	if err != nil {
		middleware.Logger.Error("get savings balance failed",
			"org_id", orgID, "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load savings balance"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"preference": pref,
		"balance":    balance,
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

// AddBill — POST /api/v1/worker/bills. Records a recurring bill so the
// product can name a payday/due-date timing mismatch explicitly.
func (h *AdvanceHandler) AddBill(c *gin.Context) {
	var req struct {
		Name   string     `json:"name"`
		Amount money.Kobo `json:"amount"`
		DueDay int        `json:"due_day"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	bill, err := h.ewa.AddBill(c.Request.Context(), orgID, employeeID, req.Name, req.Amount, req.DueDay)
	if err != nil {
		if errors.Is(err, services.ErrInvalidBill) {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		middleware.Logger.Error("add bill failed", "org_id", orgID, "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to add bill"})
		return
	}
	c.JSON(http.StatusCreated, bill)
}

// GetBills — GET /api/v1/worker/bills; the worker's own recorded bills.
func (h *AdvanceHandler) GetBills(c *gin.Context) {
	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	bills, err := h.ewa.ListBills(c.Request.Context(), orgID, employeeID)
	if err != nil {
		middleware.Logger.Error("list bills failed", "org_id", orgID, "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load bills"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": bills, "total": len(bills)})
}

// RemoveBill — DELETE /api/v1/worker/bills/:id.
func (h *AdvanceHandler) RemoveBill(c *gin.Context) {
	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)
	billID := c.Param("id")

	if err := h.ewa.RemoveBill(c.Request.Context(), orgID, employeeID, billID); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "bill not found"})
			return
		}
		middleware.Logger.Error("remove bill failed", "org_id", orgID, "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to remove bill"})
		return
	}
	c.Status(http.StatusNoContent)
}

// GetBillTiming — GET /api/v1/worker/bill-timing. Lays the worker's
// recorded bills out against their next payday, flagging which fall before
// it — a timing mismatch, not necessarily a shortfall — and which of those
// their currently available EWA draw could actually close.
func (h *AdvanceHandler) GetBillTiming(c *gin.Context) {
	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	plan, err := h.ewa.GetBillTimingPlan(c.Request.Context(), orgID, employeeID, time.Now())
	if err != nil {
		middleware.Logger.Error("bill timing plan failed", "org_id", orgID, "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build bill timing plan"})
		return
	}
	c.JSON(http.StatusOK, plan)
}
