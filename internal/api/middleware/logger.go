package middleware

import (
	"log/slog"
	"os"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// piiFields — JSON field names that must never appear in logs; extend as the model grows.
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

// Logger — global structured JSON logger for the usual aggregators to chew on.
var Logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
	Level: slog.LevelInfo,
}))

// RequestLogger — one structured log line per request, with a request_id injected for correlation.
func RequestLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		requestID := uuid.New().String()

		// Inject request_id so handlers can retrieve it for correlated logging.
		c.Set("request_id", requestID)
		c.Header("X-Request-ID", requestID)

		c.Next()

		// One log line per request; sensitive headers never make it in.
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

// RedactPII — returns "[REDACTED]" for PII fields, the raw value otherwise.
func RedactPII(field, value string) string {
	if piiFields[field] {
		return "[REDACTED]"
	}
	return value
}
