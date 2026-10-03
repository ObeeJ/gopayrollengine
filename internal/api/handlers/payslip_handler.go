package handlers

import (
	"net/http"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/services"

	"github.com/gin-gonic/gin"
)

// PayslipHandler — a worker's view of what they have been paid.
type PayslipHandler struct {
	svc *services.PayrollService
}

// NewPayslipHandler wires the handler to the payroll service.
func NewPayslipHandler(svc *services.PayrollService) *PayslipHandler {
	return &PayslipHandler{svc: svc}
}

// ListPayslips — GET /api/v1/worker/payslips. The worker's own payroll lines,
// newest first: period, what was paid, how it was made up, and whether the
// money has landed. The employee is the one in the token; nothing in the
// request can name another.
func (h *PayslipHandler) ListPayslips(c *gin.Context) {
	page, pageSize := pageParams(c)
	orgID, employeeID := middleware.OrgID(c), middleware.EmployeeID(c)
	slips, total, err := h.svc.ListPayslips(c.Request.Context(), orgID, employeeID, page, pageSize)
	if err != nil {
		middleware.Logger.Error("payslip list failed", "org_id", orgID, "employee_id", employeeID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load payslips"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"data": slips, "page": page, "page_size": pageSize, "total": total})
}
