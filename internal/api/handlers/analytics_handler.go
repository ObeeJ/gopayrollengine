package handlers

import (
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/services"
	"net/http"

	"github.com/gin-gonic/gin"
)

type AnalyticsHandler struct {
	Service *services.AnalyticsService
}

// GetPredictiveAnalytics — cash flow forecast scoped to the caller's org.
func (h *AnalyticsHandler) GetPredictiveAnalytics(c *gin.Context) {
	result, err := h.Service.GetPredictiveCashFlow(middleware.OrgID(c))
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, result)
}
