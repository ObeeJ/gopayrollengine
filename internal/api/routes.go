package api

import (
	"go-payroll-engine/internal/api/handlers"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/integrations/monnify"
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
	empRepo := repository.NewEmployeeRepository(models.DB)
	payrollRepo := repository.NewPayrollRepository(models.DB)
	orgRepo := repository.NewOrganizationRepository(models.DB)
	userRepo := repository.NewUserRepository(models.DB)
	// Handlers — dependencies injected, no handler touches models.DB directly.
	authHandler := &handlers.AuthHandler{OrgRepo: orgRepo}
	workerAuthHandler := handlers.NewWorkerAuthHandler(userRepo, empRepo)
	ewaService := services.NewEWAService()
	empHandler := handlers.NewEmployeeHandler(empRepo, ewaService)
	payrollService := services.NewPayrollService(payrollRepo, empRepo)
	payrollHandler := &handlers.PayrollHandler{Service: payrollService}
	analyticsHandler := &handlers.AnalyticsHandler{Service: services.NewAnalyticsService(payrollRepo, empRepo)}
	advanceHandler := handlers.NewAdvanceHandler(ewaService)
	policyHandler := handlers.NewPolicyHandler(ewaService)
	payrollPolicyHandler := handlers.NewPayrollPolicyHandler(payrollService)
	timeEntryHandler := handlers.NewTimeEntryHandler(services.NewTimeEntryService())
	fundingHandler := handlers.NewFundingHandler(services.NewFundingService(monnify.NewClient()))
	webhookHandler := &handlers.WebhookHandler{}
	consentHandler := &handlers.ConsentHandler{}
	complianceHandler := &handlers.ComplianceHandler{}
	dashboardHandler := handlers.NewDashboardHandler(services.NewEmployerDashboardService(empRepo))

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

		// D2C debit-collection webhook — no real aggregator wired yet, so no
		// signature verification either; see D2CDebitWebhookPayload's doc
		// comment. Add one before wiring this to a live provider.
		v1.POST("/webhooks/d2c-debit-collection", webhookHandler.HandleD2CDebitWebhook)

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
				employees.POST("/:id/terminate", middleware.RequireRole("admin"), empHandler.TerminateEmployee)
				employees.POST("/:id/hardship-grants", middleware.RequireRole("admin"), empHandler.IssueHardshipGrant)
				employees.GET("/:id/hardship-grants", empHandler.GetHardshipGrants)
			}

			payrolls := employer.Group("/payrolls")
			{
				payrolls.POST("/", middleware.RequireRole("admin"), middleware.Idempotency(workers.RDB), payrollHandler.CreatePayroll)
				payrolls.GET("/:id", payrollHandler.GetPayroll)
			}

			analytics := employer.Group("/analytics")
			{
				analytics.GET("/predictive", analyticsHandler.GetPredictiveAnalytics)
				analytics.GET("/workforce-dependency", dashboardHandler.GetWorkforceDependency)
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

			// Timesheet review — any employer role may view the queue, only
			// admin may resolve it, matching the employee/payroll write gate.
			timeEntries := employer.Group("/time-entries")
			{
				timeEntries.GET("/", timeEntryHandler.ListPendingTimeEntries)
				timeEntries.POST("/:id/approve", middleware.RequireRole("admin"), timeEntryHandler.ApproveTimeEntry)
				timeEntries.POST("/:id/reject", middleware.RequireRole("admin"), timeEntryHandler.RejectTimeEntry)
			}

			fundingAccount := employer.Group("/funding-account")
			{
				fundingAccount.GET("/", fundingHandler.GetFundingStatus)
				fundingAccount.POST("/", middleware.RequireRole("admin"), fundingHandler.ProvisionFundingAccount)
			}

			policy := employer.Group("/policy")
			{
				policy.GET("/", policyHandler.GetPolicy)
				policy.PUT("/", middleware.RequireRole("admin"), policyHandler.UpdatePolicy)
			}

			payrollPolicy := employer.Group("/payroll-policy")
			{
				payrollPolicy.GET("/", payrollPolicyHandler.GetPayrollPolicy)
				payrollPolicy.PUT("/", middleware.RequireRole("admin"), payrollPolicyHandler.UpdatePayrollPolicy)
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
			worker.POST("/protected-payday", advanceHandler.SetProtectedPayday)
			worker.GET("/savings", advanceHandler.GetSavings)
			worker.POST("/savings", advanceHandler.SetSavingsPreference)
			worker.GET("/bills", advanceHandler.GetBills)
			worker.POST("/bills", advanceHandler.AddBill)
			worker.DELETE("/bills/:id", advanceHandler.RemoveBill)
			worker.GET("/bill-timing", advanceHandler.GetBillTiming)
			worker.POST("/time-entries", timeEntryHandler.SubmitTimeEntry)
			worker.GET("/time-entries", timeEntryHandler.GetTimeEntries)
		}
	}

	return r
}
