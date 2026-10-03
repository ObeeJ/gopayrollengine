package api

import (
	"go-payroll-engine/internal/api/handlers"
	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/appenv"
	"go-payroll-engine/internal/integrations/banklink"
	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/internal/workers"
	"log"
	"os"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// SetupRouter — composition root; repositories are born here and injected everywhere else.
func SetupRouter() *gin.Engine {
	r := gin.New()
	configureTrustedProxies(r)

	// Global stack — order is load-bearing: security before logging, logging before throttle.
	r.Use(middleware.SecurityHeaders())
	r.Use(middleware.BodySizeLimit())
	r.Use(middleware.RequestLogger())
	r.Use(middleware.PrometheusMiddleware())
	r.Use(middleware.RateLimit())
	r.Use(gin.Recovery())

	// Per-tenant series: bearer-token gated. Unset outside development means
	// closed (404), never open.
	r.GET("/metrics", middleware.MetricsAuth(os.Getenv("METRICS_TOKEN"), appenv.AllowsInsecureDefaults()),
		gin.WrapH(promhttp.Handler()))

	healthHandler := &handlers.HealthHandler{DB: models.DB, RDB: workers.RDB}
	r.GET("/healthz", healthHandler.Liveness)
	r.GET("/readyz", healthHandler.Readiness)

	// Repositories — one instance each, injected down the chain.
	empRepo := repository.NewEmployeeRepository(models.DB)
	payrollRepo := repository.NewPayrollRepository(models.DB)
	orgRepo := repository.NewOrganizationRepository(models.DB)
	userRepo := repository.NewUserRepository(models.DB)
	// Handlers — dependencies injected, no handler touches models.DB directly.
	employerUsers := services.NewEmployerUserService()
	authHandler := &handlers.AuthHandler{OrgRepo: orgRepo, Users: employerUsers}
	employerUserHandler := &handlers.EmployerUserHandler{Users: employerUsers}
	// Worker login is OTP-only. No real SMS provider is integrated yet, so
	// outside MOCK_MODE the sender is nil and both worker-auth endpoints
	// answer 503 — refusing logins beats the previous behaviour of
	// accepting any code for any phone number. Wire a real OTPSender here
	// before launching the worker app.
	var otpSender services.OTPSender
	if os.Getenv("MOCK_MODE") == "true" {
		otpSender = services.LogOTPSender{}
	}
	workerAuthHandler := handlers.NewWorkerAuthHandler(userRepo, empRepo, services.NewOTPService(workers.RDB, otpSender))
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
	payslipHandler := handlers.NewPayslipHandler(payrollService)
	analyticsHandler := &handlers.AnalyticsHandler{Service: services.NewAnalyticsService(payrollRepo, empRepo)}
	advanceHandler := handlers.NewAdvanceHandler(ewaService)
	policyHandler := handlers.NewPolicyHandler(ewaService)
	payrollPolicyHandler := handlers.NewPayrollPolicyHandler(payrollService)
	timeEntryHandler := handlers.NewTimeEntryHandler(services.NewTimeEntryService())
	fundingHandler := handlers.NewFundingHandler(services.NewFundingService(monnify.NewClient()))
	webhookHandler := &handlers.WebhookHandler{}
	consentHandler := &handlers.ConsentHandler{}
	complianceHandler := &handlers.ComplianceHandler{}
	d2cHandler := &handlers.D2CHandler{BankLinkUnavailable: d2cProvider == nil}
	dashboardHandler := handlers.NewDashboardHandler(services.NewEmployerDashboardService(empRepo))

	v1 := r.Group("/api/v1")
	{
		// Public auth — employer login + token refresh.
		auth := v1.Group("/auth")
		{
			auth.POST("/login", middleware.AuthRateLimit(), authHandler.Login)
			auth.POST("/refresh", middleware.JWTAuth(), middleware.RequireEmployer(), authHandler.RefreshToken)
			auth.POST("/password", middleware.AuthRateLimit(), middleware.JWTAuth(), middleware.RequireEmployer(), middleware.RequireActiveEmployerUser(employerUsers.IsActive), authHandler.ChangePassword)
		}

		// Worker auth — OTP login, issues employee-scoped JWT.
		workerAuth := v1.Group("/worker/auth")
		{
			workerAuth.Use(middleware.AuthRateLimit())
			workerAuth.POST("/otp", workerAuthHandler.RequestOTP)
			workerAuth.POST("/login", workerAuthHandler.WorkerLogin)
		}

		// Monnify webhook — HMAC-verified, no JWT needed.
		v1.POST("/webhooks/monnify", webhookHandler.HandleMonnifyWebhook)

		// D2C debit-collection webhook — unauthenticated (no real aggregator
		// exists yet to define a signature scheme; see
		// D2CDebitWebhookPayload's doc comment), and it writes ledger
		// entries: a "successful" callback records cash received against a
		// worker's advance. So it only exists where a debit provider does —
		// under MOCK_MODE today. Registering it unconditionally let anyone
		// on the internet post a fabricated collection outcome to a
		// production deployment. Add signature verification before
		// removing this gate for a live provider.
		if d2cProvider != nil {
			v1.POST("/webhooks/d2c-debit-collection", webhookHandler.HandleD2CDebitWebhook)
		}

		// D2C signup — public, same posture as /auth/login and
		// /worker/auth/login: there is no identity yet to gate this behind.
		v1.POST("/d2c/signup", middleware.AuthRateLimit(), d2cHandler.Signup)

		// Employer routes — JWT → tenant → residency → employer gate → role gate.
		employer := v1.Group("/")
		employer.Use(middleware.JWTAuth())
		employer.Use(middleware.TenantMiddleware())
		employer.Use(middleware.DataResidency())
		employer.Use(middleware.RequireEmployer())
		employer.Use(middleware.RequireActiveEmployerUser(employerUsers.IsActive))
		employer.Use(middleware.RequirePasswordChanged())
		{
			users := employer.Group("/users", middleware.RequireRole("admin"))
			{
				users.POST("/", employerUserHandler.Create)
				users.GET("/", employerUserHandler.List)
				users.PATCH("/:id", employerUserHandler.Update)
				users.POST("/:id/reset-password", employerUserHandler.ResetPassword)
			}

			employees := employer.Group("/employees")
			{
				employees.POST("/", middleware.RequireRole("admin"), middleware.Idempotency(workers.RDB), empHandler.CreateEmployee)
				employees.GET("/", empHandler.GetEmployees)
				employees.PATCH("/:id", middleware.RequireRole("admin"), empHandler.UpdateEmployee)
				employees.POST("/:id/terminate", middleware.RequireRole("admin"), empHandler.TerminateEmployee)
				employees.POST("/:id/hardship-grants", middleware.RequireRole("admin"), middleware.Idempotency(workers.RDB), empHandler.IssueHardshipGrant)
				employees.GET("/:id/hardship-grants", empHandler.GetHardshipGrants)
			}

			payrolls := employer.Group("/payrolls")
			{
				payrolls.POST("/", middleware.RequireRole("admin"), middleware.Idempotency(workers.RDB), payrollHandler.CreatePayroll)
				payrolls.GET("/", payrollHandler.ListPayrolls)
				payrolls.GET("/:id", payrollHandler.GetPayroll)
				payrolls.POST("/:id/retry", middleware.RequireRole("admin"), middleware.Idempotency(workers.RDB), payrollHandler.RetryPayroll)
				payrolls.POST("/:id/items/:item_id/resolve", middleware.RequireRole("admin"), middleware.Idempotency(workers.RDB), payrollHandler.ResolvePayrollItem)
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
		worker.Use(middleware.RequireActiveWorker(func(employeeID string) (bool, error) {
			u, err := userRepo.FindByEmployeeID(employeeID)
			if err != nil {
				return false, err
			}
			return u.IsActive, nil
		}))
		{
			worker.GET("/wages", advanceHandler.GetEarnedWages)
			worker.GET("/payslips", payslipHandler.ListPayslips)
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

			// D2C bank-link + debit-mandate — registered unconditionally so
			// the API is self-describing: a real client gets a clean 503
			// from D2CBankLinkHandler's own nil-provider check, not a bare
			// route-not-found. d2cProvider is only ever a real provider
			// under MOCK_MODE today — there's no real aggregator (Mono/
			// Okra) wired to banklink.DebitProvider yet, and exposing a
			// Mock-backed linking flow to a real user in production would
			// let them "link" an account and get back canned success, the
			// same reason cmd/api/main.go's collect-d2c-debits mode refuses
			// to run outside MOCK_MODE. D2CHandler.Signup above already
			// refuses new D2C signups while d2cProvider is nil, so this
			// gap only matters for accounts that signed up back when a
			// provider was configured and then it was removed.
			d2cBankLinkHandler := handlers.NewD2CBankLinkHandler(d2cProvider)
			d2cBankLink := worker.Group("/d2c/bank-link")
			{
				d2cBankLink.POST("/initiate", d2cBankLinkHandler.InitiateLink)
				d2cBankLink.POST("/complete", d2cBankLinkHandler.CompleteLink)
				d2cBankLink.POST("/authorize-debit", d2cBankLinkHandler.AuthorizeDebit)
				d2cBankLink.POST("/revoke", d2cBankLinkHandler.Revoke)
				d2cBankLink.POST("/revoke-debit-mandate", d2cBankLinkHandler.RevokeDebitMandate)
			}
		}
	}

	return r
}

// configureTrustedProxies — gin trusts every proxy by default, which makes
// c.ClientIP() whatever the caller writes in X-Forwarded-For: a rate-limit
// bypass, and a forged IP in every audit and consent record. Trust none
// unless TRUSTED_PROXIES (comma-separated IPs/CIDRs of the TLS-terminating
// proxy in front of the API) says otherwise.
func configureTrustedProxies(r *gin.Engine) {
	var proxies []string
	for _, p := range strings.Split(os.Getenv("TRUSTED_PROXIES"), ",") {
		if p = strings.TrimSpace(p); p != "" {
			proxies = append(proxies, p)
		}
	}
	if err := r.SetTrustedProxies(proxies); err != nil {
		log.Fatalf("FATAL: invalid TRUSTED_PROXIES: %v", err)
	}
}
