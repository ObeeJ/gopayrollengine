package handlers

import (
	"errors"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/observability"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/services"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// WorkerAuthHandler — OTP-based login for workers; issues employee-scoped JWTs.
type WorkerAuthHandler struct {
	userRepo     repository.UserRepository
	employeeRepo repository.EmployeeRepository
	otp          *services.OTPService
}

// NewWorkerAuthHandler — wires up the handler with its dependencies. otp may
// have no sender configured; both endpoints then answer 503 rather than
// letting anyone in.
func NewWorkerAuthHandler(ur repository.UserRepository, er repository.EmployeeRepository, otp *services.OTPService) *WorkerAuthHandler {
	return &WorkerAuthHandler{userRepo: ur, employeeRepo: er, otp: otp}
}

// RequestOTP — POST /api/v1/worker/auth/otp; sends a login code to the phone.
// Answers 202 whether or not the phone belongs to an active worker, so the
// endpoint can't be used to enumerate accounts.
func (h *WorkerAuthHandler) RequestOTP(c *gin.Context) {
	var req struct {
		Phone string `json:"phone" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone is required"})
		return
	}
	if !h.otp.Available() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "worker login is not available"})
		return
	}

	user, err := h.userRepo.FindByPhone(req.Phone)
	deliver := err == nil && user.IsActive
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		middleware.Logger.Error("otp user lookup failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not send code"})
		return
	}

	switch err := h.otp.Issue(c.Request.Context(), req.Phone, deliver); {
	case errors.Is(err, services.ErrOTPRateLimited):
		c.Header("Retry-After", "60")
		c.JSON(http.StatusTooManyRequests, gin.H{"error": "too many code requests; wait before retrying"})
	case err != nil:
		middleware.Logger.Error("otp issue failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not send code"})
	default:
		c.JSON(http.StatusAccepted, gin.H{"message": "if this phone belongs to an active worker, a code has been sent"})
	}
}

// WorkerLogin — POST /api/v1/worker/auth/login; phone + OTP, issues employee-scoped JWT.
func (h *WorkerAuthHandler) WorkerLogin(c *gin.Context) {
	var req struct {
		Phone string `json:"phone" binding:"required"`
		OTP   string `json:"otp" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "phone and otp are required"})
		return
	}
	if !h.otp.Available() {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "worker login is not available"})
		return
	}

	// The code is checked first: codes are only ever issued to active
	// workers, so an unknown phone, a suspended account and a wrong code all
	// fail here identically — one response, no account-state oracle.
	ok, err := h.otp.Verify(c.Request.Context(), req.Phone, req.OTP)
	if err != nil {
		middleware.Logger.Error("otp verify failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not verify code"})
		return
	}
	if !ok {
		observability.AuthFailuresTotal.WithLabelValues("otp_invalid").Inc()
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	// Re-check after the code: the account may have been suspended or the
	// worker terminated between the code being sent and being used.
	user, err := h.userRepo.FindByPhone(req.Phone)
	if err != nil || !user.IsActive {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

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
