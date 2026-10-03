package handlers

import (
	"errors"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/observability"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/services"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

type AuthHandler struct {
	OrgRepo repository.OrganizationRepository
	Users   *services.EmployerUserService
}

// dummyPasswordHash — compared against when the org doesn't exist, so an
// unknown org_id costs the same bcrypt work as a wrong password. Without it,
// response time alone told an attacker which org IDs are real.
var dummyPasswordHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("timing-equaliser"), models.PasswordCost)
	if err != nil {
		panic("auth: dummy hash: " + err.Error())
	}
	return h
}()

// Login handles POST /api/v1/auth/login. Two shapes:
//   - {email, password}: a named person (preferred), token carries their id;
//   - {org_id, password}: the legacy shared org password, kept so existing
//     integrations keep working.
func (h *AuthHandler) Login(c *gin.Context) {
	var req struct {
		Email    string `json:"email"`
		OrgID    string `json:"org_id"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || (req.Email == "" && req.OrgID == "") {
		c.JSON(http.StatusBadRequest, gin.H{"error": "email (or org_id) and password are required"})
		return
	}
	if req.Email != "" {
		h.loginUser(c, req.Email, req.Password)
		return
	}

	org, err := h.OrgRepo.FindByID(req.OrgID)
	if err != nil {
		_ = bcrypt.CompareHashAndPassword(dummyPasswordHash, []byte(req.Password))
		observability.AuthFailuresTotal.WithLabelValues("password_invalid").Inc()
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(org.PasswordHash), []byte(req.Password)); err != nil {
		observability.AuthFailuresTotal.WithLabelValues("password_invalid").Inc()
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	// Checked after the password, with the same response, so a deactivated
	// org is indistinguishable from bad credentials.
	if !org.IsActive || org.IsD2C {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	token, err := middleware.IssueToken(org.ID, org.Role, 8*time.Hour)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not issue token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"expires_in": "8h",
		"org_id":     org.ID,
		"role":       org.Role,
	})
}

func (h *AuthHandler) loginUser(c *gin.Context, email, password string) {
	u, err := h.Users.Authenticate(c.Request.Context(), email, password)
	if err != nil {
		if errors.Is(err, services.ErrEmployerUserNotFound) {
			observability.AuthFailuresTotal.WithLabelValues("password_invalid").Inc()
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
			return
		}
		middleware.Logger.Error("employer login failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "login failed"})
		return
	}
	token, err := middleware.IssueUserToken(u.OrganizationID, u.ID, u.Role, u.MustChangePassword, time.Now(), 8*time.Hour)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not issue token"})
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"token": token, "expires_in": "8h", "org_id": u.OrganizationID, "user_id": u.ID,
		"role": u.Role, "must_change_password": u.MustChangePassword,
	})
}

// ChangePassword handles POST /api/v1/auth/password — a person sets their own
// password. Legacy org-level tokens have no person and are refused.
func (h *AuthHandler) ChangePassword(c *gin.Context) {
	userID := middleware.UserID(c)
	if userID == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "the shared organization login has no personal password; sign in with your email"})
		return
	}
	var req struct {
		CurrentPassword string `json:"current_password" binding:"required"`
		NewPassword     string `json:"new_password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBindError(c, err)
		return
	}
	err := h.Users.ChangePassword(c.Request.Context(), middleware.OrgID(c), userID, req.CurrentPassword, req.NewPassword, c.ClientIP())
	switch {
	case err == nil:
		// The old token still says must_change_password; hand back a fresh one.
		token, terr := middleware.IssueUserToken(middleware.OrgID(c), userID, middleware.Role(c), false, middleware.AuthTime(c), 8*time.Hour)
		if terr != nil {
			c.JSON(http.StatusOK, gin.H{"message": "password changed; sign in again"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"message": "password changed", "token": token})
	case errors.Is(err, services.ErrWrongPassword):
		c.JSON(http.StatusUnauthorized, gin.H{"error": "current password is incorrect"})
	case errors.Is(err, services.ErrInvalidEmployerUser):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		middleware.Logger.Error("change password failed", "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not change password"})
	}
}

// RefreshToken handles POST /api/v1/auth/refresh — renews a valid employer
// token. Employer tokens only (see routes.go), only while the org is still
// active, and never past MaxSessionAge from the original login.
func (h *AuthHandler) RefreshToken(c *gin.Context) {
	orgID := middleware.OrgID(c)
	role := middleware.Role(c)
	authTime := middleware.AuthTime(c)

	if authTime.IsZero() || time.Since(authTime) > middleware.MaxSessionAge {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired; log in again"})
		return
	}
	org, err := h.OrgRepo.FindByID(orgID)
	if err != nil || !org.IsActive {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired; log in again"})
		return
	}

	if userID := middleware.UserID(c); userID != "" {
		if ok, err := h.Users.IsActive(orgID, userID); err != nil || !ok {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "session expired; log in again"})
			return
		}
	}
	token, err := middleware.IssueUserToken(orgID, middleware.UserID(c), role, middleware.MustChange(c), authTime, 8*time.Hour)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not refresh token"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "expires_in": "8h"})
}
