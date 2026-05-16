package handlers

import (
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

type ConsentHandler struct{}

// RecordConsent handles POST /api/v1/consent.
// NDPR Article 26: records explicit employee consent before any data processing begins.
func (h *ConsentHandler) RecordConsent(c *gin.Context) {
	var req struct {
		EmployeeID  string `json:"employee_id" binding:"required"`
		ConsentType string `json:"consent_type" binding:"required"`
		Granted     bool   `json:"granted"`
		ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	record := models.ConsentRecord{
		OrganizationID: middleware.OrgID(c),
		EmployeeID:     req.EmployeeID,
		ConsentType:    req.ConsentType,
		Granted:        req.Granted,
		IPAddress:      c.ClientIP(),
		UserAgent:      c.Request.UserAgent(),
		ConsentedAt:    time.Now(),
		ExpiresAt:      req.ExpiresAt,
	}

	if err := models.DB.Create(&record).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record consent"})
		return
	}

	// Audit every consent change — regulators want to see the full consent history.
	models.AppendAudit("ConsentRecord", record.ID, "consent_recorded",
		"", req.ConsentType, c.ClientIP(), "")

	c.JSON(http.StatusCreated, record)
}

// GetConsent handles GET /api/v1/consent/:employee_id.
// Returns the full consent history for an employee — the paper trail regulators ask for.
func (h *ConsentHandler) GetConsent(c *gin.Context) {
	employeeID := c.Param("employee_id")
	var records []models.ConsentRecord
	models.ScopedDB(middleware.OrgID(c)).
		Where("employee_id = ?", employeeID).
		Order("consented_at desc").
		Find(&records)
	c.JSON(http.StatusOK, records)
}
