package handlers

import (
	"errors"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/internal/workers"
	"go-payroll-engine/pkg/money"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type EmployeeHandler struct {
	repo        repository.EmployeeRepository
	termination *services.EmployeeTerminationService
}

// NewEmployeeHandler — wires up the handler with its repository.
func NewEmployeeHandler(r repository.EmployeeRepository) *EmployeeHandler {
	return &EmployeeHandler{repo: r, termination: services.NewEmployeeTerminationService()}
}

// CreateEmployee — admin-only; employee + consent + audit commit atomically, BVN reconciles out-of-band.
func (h *EmployeeHandler) CreateEmployee(c *gin.Context) {
	var req struct {
		Name          string          `json:"name" binding:"required"`
		Email         string          `json:"email" binding:"required,email"`
		AccountNumber string          `json:"account_number" binding:"required"`
		BankCode      string          `json:"bank_code" binding:"required"`
		BVN           string          `json:"bvn" binding:"required"`
		WageType      models.WageType `json:"wage_type"` // "salaried" (default) or "hourly"

		// Exactly one of these must be set, per WageType. Not bound with
		// "required" — required would demand both regardless of wage type.
		Salary         money.Kobo `json:"salary"`
		HourlyRateKobo money.Kobo `json:"hourly_rate_kobo"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if req.WageType == "" {
		req.WageType = models.WageSalaried
	}
	switch req.WageType {
	case models.WageSalaried:
		if !req.Salary.IsPositive() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "salary is required for a salaried employee"})
			return
		}
	case models.WageHourly:
		if !req.HourlyRateKobo.IsPositive() {
			c.JSON(http.StatusBadRequest, gin.H{"error": "hourly_rate_kobo is required for an hourly employee"})
			return
		}
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "wage_type must be 'salaried' or 'hourly'"})
		return
	}

	orgID := middleware.OrgID(c)
	emp := models.Employee{
		OrganizationID: orgID,
		Name:           req.Name,
		Email:          models.EncryptedString(req.Email),
		AccountNumber:  models.EncryptedString(req.AccountNumber),
		BankCode:       models.EncryptedString(req.BankCode),
		WageType:       req.WageType,
		Salary:         req.Salary,
		HourlyRateKobo: req.HourlyRateKobo,
	}
	expires := time.Now().AddDate(1, 0, 0)

	if err := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
		if err := h.repo.WithTx(tx).Create(&emp); err != nil {
			return err
		}
		if err := tx.Create(&models.ConsentRecord{
			OrganizationID: orgID,
			EmployeeID:     emp.ID,
			ConsentType:    "payroll_processing",
			Granted:        true,
			IPAddress:      c.ClientIP(),
			UserAgent:      c.Request.UserAgent(),
			ConsentedAt:    time.Now(),
			ExpiresAt:      &expires,
		}).Error; err != nil {
			return err
		}
		return models.AppendAuditTx(tx, orgID, "Employee", emp.ID, "created",
			"", emp.Name, c.ClientIP(), "")
	}); err != nil {
		middleware.Logger.Error("employee create transaction failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create employee"})
		return
	}

	// BVN check enqueued async — Dojah latency and transient failures don't block the response.
	if err := workers.EnqueueBVNVerification(orgID, emp.ID, req.BVN); err != nil {
		middleware.Logger.Warn("BVN enqueue failed", "employee_id", emp.ID, "error", err.Error())
	}

	c.JSON(http.StatusCreated, emp)
}

// GetEmployees — paginated list scoped to the caller's org; RLS is the load-bearing fence.
func (h *EmployeeHandler) GetEmployees(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	orgID := middleware.OrgID(c)
	var (
		employees []models.Employee
		total     int64
	)
	if err := models.WithOrgScope(c.Request.Context(), orgID, func(tx *gorm.DB) error {
		var listErr error
		employees, total, listErr = h.repo.WithTx(tx).ListPaginated(orgID, page, pageSize)
		return listErr
	}); err != nil {
		middleware.Logger.Error("employee list failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list employees"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data": employees, "page": page, "page_size": pageSize, "total": total,
	})
}

// TerminateEmployee — POST /api/v1/employees/:id/terminate. Deactivates the
// employee and resolves every outstanding EWA advance: cancels any not yet
// disbursed, writes off any that were — there is no further payroll run to
// recover a disbursed advance from once employment has ended.
func (h *EmployeeHandler) TerminateEmployee(c *gin.Context) {
	var req struct {
		Reason string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	orgID := middleware.OrgID(c)
	employeeID := c.Param("id")

	emp, err := h.termination.Terminate(c.Request.Context(), orgID, employeeID, req.Reason, c.ClientIP())
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "employee not found"})
			return
		}
		middleware.Logger.Error("employee termination failed", "org_id", orgID, "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to terminate employee"})
		return
	}
	c.JSON(http.StatusOK, emp)
}
