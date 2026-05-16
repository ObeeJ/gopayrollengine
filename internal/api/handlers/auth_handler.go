package handlers

import (
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"golang.org/x/crypto/bcrypt"
)

type AuthHandler struct{}

// Login handles POST /api/v1/auth/login.
// Validates org credentials and returns a signed JWT — the key to the kingdom, time-limited.
func (h *AuthHandler) Login(c *gin.Context) {
	var req struct {
		OrgID    string `json:"org_id" binding:"required"`
		Password string `json:"password" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	var org models.Organization
	if err := models.DB.First(&org, "id = ?", req.OrgID).Error; err != nil {
		// Same error for wrong ID or wrong password — no oracle for attackers.
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(org.PasswordHash), []byte(req.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid credentials"})
		return
	}

	// 8-hour token — long enough for a workday, short enough to limit blast radius.
	token, err := middleware.IssueToken(org.ID, org.Role, 8*time.Hour)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not issue token"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"token":      token,
		"expires_in": "8h",
		"org_id":     org.ID,
	})
}

// RefreshToken handles POST /api/v1/auth/refresh.
// Issues a fresh token from a still-valid one — no password re-entry needed within the window.
func (h *AuthHandler) RefreshToken(c *gin.Context) {
	orgID := middleware.OrgID(c)
	role := middleware.Role(c)

	token, err := middleware.IssueToken(orgID, role, 8*time.Hour)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "could not refresh token"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "expires_in": "8h"})
}
