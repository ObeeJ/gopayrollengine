package handlers

import (
	"go-payroll-engine/internal/services"
	"net/http"

	"github.com/gin-gonic/gin"
)

type AnalyticsHandler struct {
	Service *services.AnalyticsService
}

// GetPredictiveAnalytics handles GET /api/v1/analytics/predictive.
// It delegates to AnalyticsService which calculates the predicted next payroll
// cost and compares it against the live Monnify wallet balance to return a
// risk assessment (Low / Medium / High).
func (h *AnalyticsHandler) GetPredictiveAnalytics(c *gin.Context) {
	result, err := h.Service.GetPredictiveCashFlow()
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, result)
}
