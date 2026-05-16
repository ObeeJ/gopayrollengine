package api

import (
	"go-payroll-engine/internal/api/handlers"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/internal/workers"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// SetupRouter — the front door; everything that enters the building goes through this lobby.
func SetupRouter() *gin.Engine {
	r := gin.New() // gin.New() not gin.Default() — we choose our own middleware adventure.

	// Global stack (order matters): security → body limit → logging → metrics → throttle → recovery.
	r.Use(middleware.SecurityHeaders())
	r.Use(middleware.BodySizeLimit())
	r.Use(middleware.RequestLogger())
	r.Use(middleware.PrometheusMiddleware()) // must run after RequestLogger so request_id is set
	r.Use(middleware.RateLimit())
	r.Use(gin.Recovery())

	// Bloom filter: 100k bits, 7 hashes → ~1% false positive rate, ~12 KB Redis memory.
	middleware.WebhookBloom = middleware.NewBloomFilter(workers.RDB, "bloom:webhooks", 100_000, 7)

	// /metrics — Prometheus scrape endpoint; restrict to internal network in production.
	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	// Health probes — no auth, no tenant scope; just "are you alive?" and "are you ready?".
	healthHandler := &handlers.HealthHandler{DB: models.DB, RDB: workers.RDB}
	r.GET("/healthz", healthHandler.Liveness)
	r.GET("/readyz", healthHandler.Readiness)

	authHandler := &handlers.AuthHandler{}
	empHandler := &handlers.EmployeeHandler{}
	payrollHandler := &handlers.PayrollHandler{Service: &services.PayrollService{}}
	analyticsHandler := &handlers.AnalyticsHandler{Service: services.NewAnalyticsService()}
	webhookHandler := &handlers.WebhookHandler{}
	consentHandler := &handlers.ConsentHandler{}
	complianceHandler := &handlers.ComplianceHandler{}

	v1 := r.Group("/api/v1")
	{
		// Auth — public; issues the JWT that unlocks everything else.
		auth := v1.Group("/auth")
		{
			auth.POST("/login", authHandler.Login)
			auth.POST("/refresh", middleware.JWTAuth(), authHandler.RefreshToken)
		}

		// Webhook is public — Monnify doesn't send a JWT, it sends an HMAC signature.
		v1.POST("/webhooks/monnify", webhookHandler.HandleMonnifyWebhook)

		// Protected: JWT → tenant identity → data residency → business logic.
		protected := v1.Group("/")
		protected.Use(middleware.JWTAuth())
		protected.Use(middleware.TenantMiddleware())
		protected.Use(middleware.DataResidency()) // rejects cross-region requests after org is loaded
		{
			employees := protected.Group("/employees")
			{
				employees.POST("/", middleware.Idempotency(workers.RDB), empHandler.CreateEmployee)
				employees.GET("/", empHandler.GetEmployees)
			}

			payrolls := protected.Group("/payrolls")
			{
				payrolls.POST("/", middleware.Idempotency(workers.RDB), payrollHandler.CreatePayroll)
				payrolls.GET("/:id", payrollHandler.GetPayroll)
			}

			analytics := protected.Group("/analytics")
			{
				analytics.GET("/predictive", analyticsHandler.GetPredictiveAnalytics)
			}

			consent := protected.Group("/consent")
			{
				consent.POST("/", consentHandler.RecordConsent)
				consent.GET("/:employee_id", consentHandler.GetConsent)
			}

			// Compliance report — role-gated; only compliance officers can pull this.
			compliance := protected.Group("/compliance")
			compliance.Use(middleware.RequireRole("compliance"))
			{
				compliance.GET("/report", complianceHandler.GetComplianceReport)
			}
		}
	}

	return r
}
