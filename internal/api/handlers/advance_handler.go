package handlers

import (
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"net/http"

	"github.com/gin-gonic/gin"
)

// AdvanceHandler — worker-facing EWA endpoints; every query is fenced by employee_id from JWT.
type AdvanceHandler struct {
	employeeRepo repository.EmployeeRepository
}

// NewAdvanceHandler — wires up the handler.
func NewAdvanceHandler(er repository.EmployeeRepository) *AdvanceHandler {
	return &AdvanceHandler{employeeRepo: er}
}

// GetEarnedWages handles GET /api/v1/worker/wages — returns the worker's accrued wage snapshot.
func (h *AdvanceHandler) GetEarnedWages(c *gin.Context) {
	employeeID := middleware.EmployeeID(c)
	orgID := middleware.OrgID(c)

	emp, err := h.employeeRepo.FindByID(orgID, employeeID)
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

// RequestAdvance handles POST /api/v1/worker/advances — creates an advance request.
func (h *AdvanceHandler) RequestAdvance(c *gin.Context) {
	var req struct {
		Amount float64 `json:"amount" binding:"required,gt=0"`
		Reason string  `json:"reason" binding:"required"`
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

	if err := models.DB.Create(&advance).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create advance request"})
		return
	}

	models.AppendAudit("AdvanceRequest", advance.ID, "created", "", req.Reason, c.ClientIP(), "")
	c.JSON(http.StatusAccepted, advance)
}

// GetAdvanceHistory handles GET /api/v1/worker/advances — worker sees only their own history.
func (h *AdvanceHandler) GetAdvanceHistory(c *gin.Context) {
	employeeID := middleware.EmployeeID(c)

	var advances []models.AdvanceRequest
	models.DB.Where("employee_id = ?", employeeID).Order("created_at desc").Find(&advances)

	c.JSON(http.StatusOK, gin.H{"data": advances, "total": len(advances)})
}
