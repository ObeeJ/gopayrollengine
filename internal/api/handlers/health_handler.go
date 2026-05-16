package handlers

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

// HealthHandler — the system's vital signs monitor; Kubernetes and load balancers live by these.
type HealthHandler struct {
	DB  *gorm.DB
	RDB *redis.Client
}

// Liveness handles GET /healthz — "is the process alive?" If this fails, restart the container.
func (h *HealthHandler) Liveness(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"status":  "ok",
		"version": os.Getenv("APP_VERSION"),
		"env":     os.Getenv("APP_ENV"),
	})
}

// Readiness handles GET /readyz — "is the process ready to serve traffic?"
// Checks DB + Redis; fails gracefully so the load balancer drains this instance without a restart.
func (h *HealthHandler) Readiness(c *gin.Context) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	checks := map[string]string{}
	healthy := true

	// PostgreSQL — the source of truth; without it nothing works.
	sqlDB, err := h.DB.DB()
	if err != nil || sqlDB.PingContext(ctx) != nil {
		checks["postgres"] = "unreachable"
		healthy = false
	} else {
		checks["postgres"] = "ok"
	}

	// Redis — idempotency, rate limiting, and bloom filter all depend on this.
	if err := h.RDB.Ping(ctx).Err(); err != nil {
		checks["redis"] = "unreachable"
		healthy = false
	} else {
		checks["redis"] = "ok"
	}

	// Encryption — if the KEK is missing, PII writes will fail silently.
	if os.Getenv("ENCRYPTION_KEK") == "" && os.Getenv("APP_ENV") == "production" {
		checks["encryption"] = "kek_missing"
		healthy = false
	} else {
		checks["encryption"] = "ok"
	}

	status := http.StatusOK
	statusStr := "ready"
	if !healthy {
		status = http.StatusServiceUnavailable
		statusStr = "degraded"
	}

	c.JSON(status, gin.H{
		"status": statusStr,
		"checks": checks,
		"time":   time.Now().UTC(),
	})
}
