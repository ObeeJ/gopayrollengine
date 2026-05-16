package middleware

import (
	"log/slog"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// piiFields is a Hash Set of JSON field names that must never appear in logs.
// DSA: Hash Set (map[string]bool) — O(1) membership test per field name.
// Extend this set as new PII fields are added to the data model.
var piiFields = map[string]bool{
	"account_number": true,
	"bank_code":      true,
	"email":          true,
	"bvn":            true,
	"phone":          true,
	"name":           true,
	"password":       true,
	"secret":         true,
	"token":          true,
	"authorization":  true,
}

// Logger is the global structured logger. JSON format for machine parsing
// in CloudWatch, Datadog, or Grafana Loki.
var Logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
	Level: slog.LevelInfo,
}))

// RequestLogger is a Gin middleware that emits one structured log line per
// request. It injects a unique request_id into the Gin context so all
// downstream log calls can be correlated in a log aggregator.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		requestID := uuid.New().String()

		// Inject request_id so handlers can retrieve it for correlated logging.
		c.Set("request_id", requestID)
		c.Header("X-Request-ID", requestID)

		c.Next()

		// Emit one structured log line after the handler completes.
		// Sensitive headers are never logged — only safe metadata.
		Logger.Info("request",
			"request_id", requestID,
			"method", c.Request.Method,
			"path", c.Request.URL.Path,
			"status", c.Writer.Status(),
			"latency_ms", time.Since(start).Milliseconds(),
			"client_ip", c.ClientIP(),
			"bytes_out", c.Writer.Size(),
		)
	}
}

// RedactPII checks a field name against the PII hash set and returns
// "[REDACTED]" if it matches, or the original value if safe.
// Use this before logging any user-supplied or employee data.
//
// Example:
//
//	Logger.Info("employee", "account_number", RedactPII("account_number", emp.AccountNumber))
func RedactPII(field, value string) string {
	if piiFields[field] {
		return "[REDACTED]"
	}
	return value
}
