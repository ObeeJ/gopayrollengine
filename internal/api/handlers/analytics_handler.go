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
	result, err := h.Service.GetPredictiveCashFlow(c.Request.Context(), middleware.OrgID(c))
	if err != nil {
		middleware.Logger.Error("predictive analytics failed", "org_id", middleware.OrgID(c), "error", err.Error())
		c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to build the forecast"})
		return
	}
	c.JSON(http.StatusOK, result)
}
