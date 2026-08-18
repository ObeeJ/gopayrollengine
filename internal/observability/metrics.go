package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Metrics auditors and SREs will both want; named <namespace>_<subsystem>_<name>_<unit>.
var (
	// HTTP layer — answers "is the API healthy and how fast is it?"
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_http_requests_total",
		Help: "Total HTTP requests by method, path, and status code.",
	}, []string{"method", "path", "status"})

	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "payroll_http_request_duration_seconds",
		Help:    "HTTP request latency distribution — p50/p95/p99 matter most.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "path"})

	// Payroll pipeline — answers "how many payrolls ran and did they succeed?"
	PayrollsCreatedTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_batches_created_total",
		Help: "Total payroll batches created, by org and status.",
	}, []string{"org_id", "status"})

	PayrollProcessingDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "payroll_processing_duration_seconds",
		Help:    "End-to-end payroll processing time from queue to Monnify submission.",
		Buckets: []float64{1, 5, 10, 30, 60, 120, 300},
	}, []string{"org_id"})

	PayrollItemsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_items_total",
		Help: "Total individual disbursements by final status.",
	}, []string{"status"})

	// Monnify integration — answers "is our payment gateway healthy?"
	MonnifyCallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "monnify_api_calls_total",
		Help: "Total Monnify API calls by operation and outcome.",
	}, []string{"operation", "success"})

	MonnifyCallDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "monnify_api_call_duration_seconds",
		Help:    "Monnify API call latency — spikes here mean Monnify is slow, not us.",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30},
	}, []string{"operation"})

	// Provider-generic — every payment rail behind the provider.Provider
	// interface reports here, labelled by provider name, so a dashboard can
	// compare Monnify against Paystack against whatever comes next without a
	// new metric per integration.
	ProviderCallsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_provider_api_calls_total",
		Help: "Total payment provider API calls by provider, operation, and outcome.",
	}, []string{"provider", "operation", "success"})

	ProviderCallDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "payroll_provider_api_call_duration_seconds",
		Help:    "Payment provider API call latency by provider and operation.",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 30},
	}, []string{"provider", "operation"})

	// Webhook pipeline — answers "are Monnify callbacks arriving and being processed?"
	WebhookEventsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_webhook_events_total",
		Help: "Total Monnify webhook events received by type and outcome.",
	}, []string{"event_type", "outcome"})

	WebhookDuplicatesTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "payroll_webhook_duplicates_total",
		Help: "Duplicate webhook events caught by bloom filter — high count = Monnify retrying.",
	})

	// Security signals — answers "is anyone trying to break in?"
	AuthFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_auth_failures_total",
		Help: "Authentication failures by type — spike here means brute force attempt.",
	}, []string{"type"}) // "jwt_invalid" | "jwt_expired" | "api_key_wrong"

	RateLimitHitsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_rate_limit_hits_total",
		Help: "Requests rejected by rate limiter — sustained spike = abuse or misconfigured client.",
	}, []string{"key_type"}) // "api_key" | "ip"

	// Worker queue — answers "is the background job system keeping up?"
	WorkerTasksTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_worker_tasks_total",
		Help: "Asynq tasks processed by type and result.",
	}, []string{"task_type", "result"}) // result: "success" | "error" | "retry"

	// BVN verification — answers "what % of employees pass KYC?"
	BVNVerificationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_bvn_verifications_total",
		Help: "BVN verification outcomes — low success rate = provider issue or data quality problem.",
	}, []string{"provider", "status"})

	// Settlement integrity — any non-zero value here is a page, not a dashboard line.
	WebhookAmountMismatchTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_webhook_amount_mismatch_total",
		Help: "Webhooks whose reported amount disagreed with the stored item amount.",
	}, []string{"org_id"})

	LedgerImbalanceTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_ledger_imbalance_total",
		Help: "Ledger transactions rejected because debits did not equal credits.",
	}, []string{"org_id"})

	// EWA — answers "is early wage access helping or trapping people?"
	EWAAdvancesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_ewa_advances_total",
		Help: "EWA advance requests by org and outcome.",
	}, []string{"org_id", "outcome"}) // "approved" | "rejected"

	EWADeclineReasonsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_ewa_decline_reasons_total",
		Help: "Why EWA requests were declined — the guardrail that fired.",
	}, []string{"org_id", "reason"})

	EWADependencyTier = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "payroll_ewa_dependency_tier_employees",
		Help: "Employees currently in each dependency tier — the product-health metric that matters most.",
	}, []string{"org_id", "tier"}) // "healthy" | "elevated" | "strained" | "dependent"

	EWAUtilizationRatio = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "payroll_ewa_utilization_ratio",
		Help:    "Fraction of earned wages drawn before payday, per approved advance.",
		Buckets: []float64{0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.75, 0.9, 1.0},
	}, []string{"org_id"})

	// EWA disbursement — answers "is the money actually moving?" An advance
	// approved without ever being disbursed is worse than one declined: the
	// worker was told yes and it never arrived.
	EWADisbursementEnqueueFailuresTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_ewa_disbursement_enqueue_failures_total",
		Help: "Approved advances whose disbursement task failed to enqueue — self-heals via idempotency-key retry, but a sustained rate means the queue is down.",
	}, []string{"org_id"})

	EWADisbursementOutcomesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "payroll_ewa_disbursement_outcomes_total",
		Help: "EWA disbursement worker outcomes by provider and result.",
	}, []string{"provider", "result"}) // result: "submitted" | "rejected" | "provider_unavailable"

	EWADisbursementDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "payroll_ewa_disbursement_duration_seconds",
		Help:    "End-to-end EWA disbursement submission time, from task pickup to provider acceptance.",
		Buckets: []float64{0.5, 1, 2, 5, 10, 30},
	}, []string{"provider"})
)
