package handlers

import (
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/services"
	"net/http"

	"github.com/gin-gonic/gin"
)

type PayrollHandler struct {
	Service *services.PayrollService
}

// CreatePayroll — 202 because the money hasn't moved yet; the worker handles that part.
func (h *PayrollHandler) CreatePayroll(c *gin.Context) {
	var req struct {
		Period string `json:"period" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	payroll, err := h.Service.CreatePayroll(middleware.OrgID(c), req.Period)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusAccepted, payroll)
}

// GetPayroll — loads the batch and all its items; one request, full picture.
func (h *PayrollHandler) GetPayroll(c *gin.Context) {
	id := c.Param("id")
	var payroll models.Payroll
	// Scoped to org — you can only see your own payrolls.
	if err := models.ScopedDB(middleware.OrgID(c)).Preload("Items").First(&payroll, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "payroll not found"})
		return
	}
	c.JSON(http.StatusOK, payroll)
}
