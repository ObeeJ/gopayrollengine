package handlers

import (
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/services"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

type EmployeeHandler struct {
	repo   repository.EmployeeRepository
	bvnSvc *services.BVNService
}

// NewEmployeeHandler — wires up the handler with its repository and BVN service.
func NewEmployeeHandler(r repository.EmployeeRepository, b *services.BVNService) *EmployeeHandler {
	return &EmployeeHandler{repo: r, bvnSvc: b}
}

// CreateEmployee — creates an employee; BVN verified, consent recorded, PII encrypted.
// Only admin role can create employees — viewers are read-only.
func (h *EmployeeHandler) CreateEmployee(c *gin.Context) {
	var req struct {
		Name          string  `json:"name" binding:"required"`
		Email         string  `json:"email" binding:"required,email"`
		AccountNumber string  `json:"account_number" binding:"required"`
		BankCode      string  `json:"bank_code" binding:"required"`
		Salary        float64 `json:"salary" binding:"required"`
		BVN           string  `json:"bvn" binding:"required"`
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

	if err := h.repo.Create(&emp); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create employee"})
		return
	}

	if _, err := h.bvnSvc.VerifyBVN(orgID, emp.ID, req.BVN); err != nil {
		middleware.Logger.Warn("BVN verification failed", "employee_id", emp.ID, "error", err.Error())
	}

	expires := time.Now().AddDate(1, 0, 0)
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

// GetEmployees — paginated list scoped to the caller's org.
// Both admin and viewer roles can read.
func (h *EmployeeHandler) GetEmployees(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	employees, total, err := h.repo.ListPaginated(middleware.OrgID(c), page, pageSize)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list employees"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": employees, "page": page, "page_size": pageSize, "total": total,
	})
}
