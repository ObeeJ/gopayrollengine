package observability

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// All metrics are registered once at package init — promauto handles the registry boilerplate.
// Naming convention: <namespace>_<subsystem>_<name>_<unit>
// These are the exact metrics a SOC 2 auditor or CBN examiner will ask to see on a dashboard.

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
)
