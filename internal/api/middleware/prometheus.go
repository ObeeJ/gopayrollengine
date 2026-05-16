package middleware

import (
	"go-payroll-engine/internal/observability"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// PrometheusMiddleware — records request count and latency for every route automatically.
// Runs after the handler so it captures the final status code, not the initial one.
func PrometheusMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.FullPath() // use the route pattern, not the raw URL — avoids high cardinality
		if path == "" {
			path = "unknown"
		}

		c.Next()

		duration := time.Since(start).Seconds()
		status := strconv.Itoa(c.Writer.Status())

		observability.HTTPRequestsTotal.WithLabelValues(c.Request.Method, path, status).Inc()
		observability.HTTPRequestDuration.WithLabelValues(c.Request.Method, path).Observe(duration)
	}
}
