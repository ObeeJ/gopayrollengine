package api

import (
	"go-payroll-engine/internal/api/handlers"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/integrations/banklink"
	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/internal/workers"
	"os"

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
	// D2C eligibility (services.EWAService.D2CProvider) reads from the same
	// banklink.Provider the bank-link endpoints below write to — one shared
	// instance, not two independent ones, the same way a real provider
	// would eventually be a single configured client. Only wired under
	// MOCK_MODE; see the longer comment on the bank-link route group below
	// for why a D2C org otherwise gets ErrD2CProviderUnavailable and blocks
	// with DeclineNoIncomeHistory rather than a fake prediction.
	var d2cProvider banklink.DebitProvider
	if os.Getenv("MOCK_MODE") == "true" {
		d2cProvider = banklink.NewMock()
		ewaService.D2CProvider = d2cProvider
	}
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
	d2cHandler := &handlers.D2CHandler{}
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

		// D2C signup — public, same posture as /auth/login and
		// /worker/auth/login: there is no identity yet to gate this behind.
		v1.POST("/d2c/signup", d2cHandler.Signup)

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

			// D2C bank-link + debit-mandate — only registered under
			// MOCK_MODE. There's no real aggregator (Mono/Okra) wired to
			// banklink.DebitProvider yet, and exposing a Mock-backed
			// linking flow to a real user in production would let them
			// "link" an account and get back canned success — the same
			// reason cmd/api/main.go's collect-d2c-debits mode refuses to
			// run outside MOCK_MODE. Remove this gate only once d2cProvider
			// above is a real provider.
			if d2cProvider != nil {
				d2cBankLinkHandler := handlers.NewD2CBankLinkHandler(d2cProvider)
				d2cBankLink := worker.Group("/d2c/bank-link")
				{
					d2cBankLink.POST("/initiate", d2cBankLinkHandler.InitiateLink)
					d2cBankLink.POST("/complete", d2cBankLinkHandler.CompleteLink)
					d2cBankLink.POST("/authorize-debit", d2cBankLinkHandler.AuthorizeDebit)
				}
			}
		}
	}

	return r
}
