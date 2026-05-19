package handlers

import (
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type ConsentHandler struct{}

// RecordConsent — POST /api/v1/consent; NDPR Art. 26, consent + audit commit atomically.
func (h *ConsentHandler) RecordConsent(c *gin.Context) {
	var req struct {
		EmployeeID  string     `json:"employee_id" binding:"required"`
		ConsentType string     `json:"consent_type" binding:"required"`
		Granted     bool       `json:"granted"`
		ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	orgID := middleware.OrgID(c)
	record := models.ConsentRecord{
		OrganizationID: orgID,
		EmployeeID:     req.EmployeeID,
		ConsentType:    req.ConsentType,
		Granted:        req.Granted,
		IPAddress:      c.ClientIP(),
		UserAgent:      c.Request.UserAgent(),
		ConsentedAt:    time.Now(),
		ExpiresAt:      req.ExpiresAt,
	}

	if err := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return models.AppendAuditTx(tx, orgID, "ConsentRecord", record.ID, "consent_recorded",
			"", req.ConsentType, c.ClientIP(), "")
	}); err != nil {
		middleware.Logger.Error("consent record + audit failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to record consent"})
		return
	}

	c.JSON(http.StatusCreated, record)
}

// GetConsent — GET /api/v1/consent/:employee_id; the paper trail regulators ask for, RLS-fenced.
func (h *ConsentHandler) GetConsent(c *gin.Context) {
	employeeID := c.Param("employee_id")
	orgID := middleware.OrgID(c)

	var records []models.ConsentRecord
	if err := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
		return tx.Where("employee_id = ?", employeeID).
			Order("consented_at desc").
			Find(&records).Error
	}); err != nil {
		middleware.Logger.Error("consent history fetch failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load consent history"})
		return
	}

	c.JSON(http.StatusOK, records)
}
