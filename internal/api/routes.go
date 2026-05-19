package api

import (
	"go-payroll-engine/internal/api/handlers"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/internal/workers"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// SetupRouter — composition root; repositories are born here and injected everywhere else.
func SetupRouter() *gin.Engine {
	r := gin.New()

	// Global stack — order is load-bearing: security before logging, logging before throttle.
	r.Use(middleware.SecurityHeaders())
	r.Use(middleware.BodySizeLimit())
	r.Use(middleware.RequestLogger())
	r.Use(middleware.PrometheusMiddleware())
	r.Use(middleware.RateLimit())
	r.Use(gin.Recovery())

	// Bloom filter: 100k bits, 7 hashes, ~1% FP rate, ~12 KB Redis.
	middleware.WebhookBloom = middleware.NewBloomFilter(workers.RDB, "bloom:webhooks", 100_000, 7)

	r.GET("/metrics", gin.WrapH(promhttp.Handler()))

	healthHandler := &handlers.HealthHandler{DB: models.DB, RDB: workers.RDB}
	r.GET("/healthz", healthHandler.Liveness)
	r.GET("/readyz", healthHandler.Readiness)

	// Repositories — one instance each, injected down the chain.
	empRepo     := repository.NewEmployeeRepository(models.DB)
	payrollRepo := repository.NewPayrollRepository(models.DB)
	orgRepo     := repository.NewOrganizationRepository(models.DB)
	userRepo    := repository.NewUserRepository(models.DB)
	// Handlers — dependencies injected, no handler touches models.DB directly.
	authHandler       := &handlers.AuthHandler{OrgRepo: orgRepo}
	workerAuthHandler := handlers.NewWorkerAuthHandler(userRepo, empRepo)
	empHandler        := handlers.NewEmployeeHandler(empRepo)
	payrollHandler    := &handlers.PayrollHandler{Service: services.NewPayrollService(payrollRepo, empRepo)}
	analyticsHandler  := &handlers.AnalyticsHandler{Service: services.NewAnalyticsService(payrollRepo, empRepo)}
	advanceHandler    := handlers.NewAdvanceHandler(empRepo)
	webhookHandler    := &handlers.WebhookHandler{}
	consentHandler    := &handlers.ConsentHandler{}
	complianceHandler := &handlers.ComplianceHandler{}

	v1 := r.Group("/api/v1")
	{
		// Public auth — employer login + token refresh.
		auth := v1.Group("/auth")
		{
			auth.POST("/login", authHandler.Login)
			auth.POST("/refresh", middleware.JWTAuth(), authHandler.RefreshToken)
		}

		// Worker auth — OTP login, issues employee-scoped JWT.
		workerAuth := v1.Group("/worker/auth")
		{
			workerAuth.POST("/login", workerAuthHandler.WorkerLogin)
		}

		// Monnify webhook — HMAC-verified, no JWT needed.
		v1.POST("/webhooks/monnify", webhookHandler.HandleMonnifyWebhook)

		// Employer routes — JWT → tenant → residency → employer gate → role gate.
		employer := v1.Group("/")
		employer.Use(middleware.JWTAuth())
		employer.Use(middleware.TenantMiddleware())
		employer.Use(middleware.DataResidency())
		employer.Use(middleware.RequireEmployer())
		{
			employees := employer.Group("/employees")
			{
				employees.POST("/", middleware.RequireRole("admin"), middleware.Idempotency(workers.RDB), empHandler.CreateEmployee)
				employees.GET("/", empHandler.GetEmployees)
			}

			payrolls := employer.Group("/payrolls")
			{
				payrolls.POST("/", middleware.RequireRole("admin"), middleware.Idempotency(workers.RDB), payrollHandler.CreatePayroll)
				payrolls.GET("/:id", payrollHandler.GetPayroll)
			}

			analytics := employer.Group("/analytics")
			{
				analytics.GET("/predictive", analyticsHandler.GetPredictiveAnalytics)
			}

			consent := employer.Group("/consent")
			{
				consent.POST("/", consentHandler.RecordConsent)
				consent.GET("/:employee_id", consentHandler.GetConsent)
			}

			compliance := employer.Group("/compliance")
			compliance.Use(middleware.RequireRole("compliance"))
			{
				compliance.GET("/report", complianceHandler.GetComplianceReport)
			}
		}

		// Worker routes — JWT → tenant → residency → worker gate; same fence as employer side.
		worker := v1.Group("/worker")
		worker.Use(middleware.JWTAuth())
		worker.Use(middleware.TenantMiddleware())
		worker.Use(middleware.DataResidency())
		worker.Use(middleware.RequireWorker())
		{
			worker.GET("/wages", advanceHandler.GetEarnedWages)
			worker.POST("/advances", advanceHandler.RequestAdvance)
			worker.GET("/advances", advanceHandler.GetAdvanceHistory)
		}
	}

	return r
}
