package handlers

import (
	"errors"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type ConsentHandler struct{}

var errConsentEmployeeNotFound = errors.New("consent: employee not found in this organization")

// RecordConsent — POST /api/v1/consent; NDPR Art. 26, consent + audit commit atomically.
func (h *ConsentHandler) RecordConsent(c *gin.Context) {
	var req struct {
		EmployeeID  string     `json:"employee_id" binding:"required"`
		ConsentType string     `json:"consent_type" binding:"required"`
		Granted     bool       `json:"granted"`
		ExpiresAt   *time.Time `json:"expires_at,omitempty"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		respondBindError(c, err)
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
		// employee_id comes from the request body, and the foreign key on
		// consent_records is checked by Postgres with row-level security
		// bypassed — so without this, naming another tenant's employee
		// passes both the FK and RLS's WITH CHECK (which only inspects the
		// caller's own organization_id). Prove ownership here. The employee
		// need not be active: withdrawing consent after leaving is a
		// legitimate NDPR request.
		var owned int64
		if err := tx.Model(&models.Employee{}).
			Where("id = ? AND organization_id = ?", req.EmployeeID, orgID).
			Count(&owned).Error; err != nil {
			return err
		}
		if owned == 0 {
			return errConsentEmployeeNotFound
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		return models.AppendAuditTx(tx, orgID, "ConsentRecord", record.ID, "consent_recorded",
			"", req.ConsentType, c.ClientIP(), "")
	}); err != nil {
		// Another tenant's employee and a nonexistent one are deliberately
		// indistinguishable: a different answer would let any employer probe
		// which employee IDs exist in other companies.
		if errors.Is(err, errConsentEmployeeNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "employee not found"})
			return
		}
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
