package handlers

import (
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/pkg/money"
	"net/http"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// AdvanceHandler — worker-facing EWA endpoints, all scoped through RLS.
type AdvanceHandler struct {
	employeeRepo repository.EmployeeRepository
}

// NewAdvanceHandler — wires up the handler.
func NewAdvanceHandler(er repository.EmployeeRepository) *AdvanceHandler {
	return &AdvanceHandler{employeeRepo: er}
}

// GetEarnedWages — GET /api/v1/worker/wages; returns the worker's accrued snapshot.
func (h *AdvanceHandler) GetEarnedWages(c *gin.Context) {
	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	var emp *models.Employee
	err := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
		var fetchErr error
		emp, fetchErr = h.employeeRepo.WithTx(tx).FindByID(orgID, employeeID)
		return fetchErr
	})
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "employee record not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"employee_id":    emp.ID,
		"monthly_salary": emp.Salary,
		"earned_to_date": nil,
		"max_advance":    nil,
	})
}

// RequestAdvance — POST /api/v1/worker/advances; advance + audit commit atomically or not at all.
func (h *AdvanceHandler) RequestAdvance(c *gin.Context) {
	var req struct {
		Amount money.Kobo `json:"amount" binding:"required,gt=0"`
		Reason string     `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	advance := models.AdvanceRequest{
		OrgID:      orgID,
		EmployeeID: employeeID,
		Amount:     req.Amount,
		Reason:     req.Reason,
		Status:     "pending",
	}

	if err := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
		if err := tx.Create(&advance).Error; err != nil {
			return err
		}
		return models.AppendAuditTx(tx, orgID, "AdvanceRequest", advance.ID, "created",
			"", req.Reason, c.ClientIP(), "")
	}); err != nil {
		middleware.Logger.Error("advance create + audit failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create advance request"})
		return
	}

	c.JSON(http.StatusAccepted, advance)
}

// GetAdvanceHistory — GET /api/v1/worker/advances; worker's own history, RLS-fenced.
func (h *AdvanceHandler) GetAdvanceHistory(c *gin.Context) {
	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	var advances []models.AdvanceRequest
	if err := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
		return tx.Where("employee_id = ?", employeeID).
			Order("created_at desc").
			Find(&advances).Error
	}); err != nil {
		middleware.Logger.Error("advance history fetch failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load advance history"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"data": advances, "total": len(advances)})
}
