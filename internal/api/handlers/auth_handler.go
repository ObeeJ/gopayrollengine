package handlers

import (
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/observability"
	"go-payroll-engine/internal/repository"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

type AuthHandler struct {
	OrgRepo repository.OrganizationRepository
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

// Login handles POST /api/v1/auth/login — validates org credentials, issues employer JWT.
func (h *AuthHandler) Login(c *gin.Context) {
	var req struct {
		OrgID    string `json:"org_id" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "org_id and password are required"})
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

	token, err := middleware.IssueRefreshedToken(orgID, role, authTime, 8*time.Hour)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not refresh token"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "expires_in": "8h"})
}
