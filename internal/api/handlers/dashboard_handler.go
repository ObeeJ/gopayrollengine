package handlers

import (
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/services"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
)

// DashboardHandler serves aggregate, anonymised workforce reporting to
// employers — never a per-worker figure. See EmployerDashboardService for
// the k-anonymity guarantee.
type DashboardHandler struct {
	service *services.EmployerDashboardService
}

// NewDashboardHandler wires up the handler with its service.
func NewDashboardHandler(s *services.EmployerDashboardService) *DashboardHandler {
	return &DashboardHandler{service: s}
}

// GetWorkforceDependency — current dependency-tier breakdown for the org.
func (h *DashboardHandler) GetWorkforceDependency(c *gin.Context) {
	orgID := middleware.OrgID(c)
	report, err := h.service.GetWorkforceDependencyReport(c.Request.Context(), orgID, time.Now())
	if err != nil {
		middleware.Logger.Error("workforce dependency report failed", "org_id", orgID, "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build workforce dependency report"})
		return
	}
	c.JSON(http.StatusOK, report)
}
