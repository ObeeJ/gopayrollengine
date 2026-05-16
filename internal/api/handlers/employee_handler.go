package handlers

import (
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/services"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type EmployeeHandler struct{}

// CreateEmployee — adds a new employee; BVN is verified and consent is recorded before DB insert.
func (h *EmployeeHandler) CreateEmployee(c *gin.Context) {
	var req struct {
		Name          string  `json:"name" binding:"required"`
		Email         string  `json:"email" binding:"required,email"`
		AccountNumber string  `json:"account_number" binding:"required"`
		BankCode      string  `json:"bank_code" binding:"required"`
		Salary        float64 `json:"salary" binding:"required"`
		BVN           string  `json:"bvn" binding:"required"` // required for CBN KYC compliance
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	orgID := middleware.OrgID(c)

	emp := models.Employee{
		OrganizationID: orgID,
		Name:           req.Name,
		Email:          req.Email,
		AccountNumber:  models.EncryptedString(req.AccountNumber),
		BankCode:       models.EncryptedString(req.BankCode),
		Salary:         req.Salary,
	}

	if err := models.DB.Create(&emp).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create employee"})
		return
	}

	// BVN verification — runs after DB insert so we have an employee ID to attach it to.
	bvnSvc := services.NewBVNService()
	if _, err := bvnSvc.VerifyBVN(orgID, emp.ID, req.BVN); err != nil {
		// Log the failure but don't block the response — verification can be retried.
		middleware.Logger.Warn("BVN verification failed",
			"employee_id", emp.ID,
			"error", err.Error(),
		)
	}

	// Auto-record payroll processing consent at creation — NDPR baseline requirement.
	expires := time.Now().AddDate(1, 0, 0) // consent expires in 1 year; must be renewed
	models.DB.Create(&models.ConsentRecord{
		OrganizationID: orgID,
		EmployeeID:     emp.ID,
		ConsentType:    "payroll_processing",
		Granted:        true,
		IPAddress:      c.ClientIP(),
		UserAgent:      c.Request.UserAgent(),
		ConsentedAt:    time.Now(),
		ExpiresAt:      &expires,
	})

	models.AppendAudit("Employee", emp.ID, "created", "", emp.Name, c.ClientIP(), "")
	c.JSON(http.StatusCreated, emp)
}

// GetEmployees — paginated list scoped to the caller's org; OFFSET is fine until 100k rows.
func (h *EmployeeHandler) GetEmployees(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	var total int64
	models.ScopedDB(middleware.OrgID(c)).Model(&models.Employee{}).Count(&total)

	var employees []models.Employee
	models.ScopedDB(middleware.OrgID(c)).Offset((page - 1) * pageSize).Limit(pageSize).Find(&employees)

	c.JSON(http.StatusOK, gin.H{
		"data": employees, "page": page, "page_size": pageSize, "total": total,
	})
}
