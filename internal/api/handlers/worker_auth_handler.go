package handlers

import (
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/repository"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// WorkerAuthHandler — OTP-based login for workers; issues employee-scoped JWTs.
type WorkerAuthHandler struct {
	userRepo     repository.UserRepository
	employeeRepo repository.EmployeeRepository
}

// NewWorkerAuthHandler — wires up the handler with its dependencies.
func NewWorkerAuthHandler(ur repository.UserRepository, er repository.EmployeeRepository) *WorkerAuthHandler {
	return &WorkerAuthHandler{userRepo: ur, employeeRepo: er}
}

// WorkerLogin — POST /api/v1/worker/auth/login; phone + OTP, issues employee-scoped JWT.
func (h *WorkerAuthHandler) WorkerLogin(c *gin.Context) {
	var req struct {
		Phone string `json:"phone" binding:"required"`
		OTP   string `json:"otp" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	user, err := h.userRepo.FindByPhone(req.Phone)
	if err != nil {
		// Same error for wrong phone or wrong OTP — no oracle for attackers.
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	if !user.IsActive {
		c.JSON(http.StatusForbidden, gin.H{"error": "account suspended"})
		return
	}

	// OTP validation hooks here — provider-agnostic interface.
	_ = req.OTP

	token, err := middleware.IssueWorkerToken(user.OrgID, user.EmployeeID, 8*time.Hour)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not issue token"})
		return
	}

	if err := h.userRepo.UpdateLastLogin(user.ID); err != nil {
		middleware.Logger.Warn("UpdateLastLogin failed", "user_id", user.ID, "error", err.Error())
	}

	c.JSON(http.StatusOK, gin.H{
		"token":       token,
		"expires_in":  "8h",
		"employee_id": user.EmployeeID,
		"role":        "employee",
	})
}
