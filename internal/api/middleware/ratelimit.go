package middleware

import (
	"go-payroll-engine/internal/observability"
	"net/http"
	"sync"

	"github.com/gin-gonic/gin"
	"golang.org/x/time/rate"
)

// tokenBucket holds a rate.Limiter per API key.
// DSA: Token Bucket — each key gets a fixed burst capacity that refills at
// a steady rate. Requests consume one token; an empty bucket returns 429.
// The map is the Hash Map that gives O(1) lookup per request.
type tokenBucket struct {
	mu      sync.Mutex
	buckets map[string]*rate.Limiter
	r       rate.Limit // tokens per second
	burst   int        // max burst size
}

var globalBucket = &tokenBucket{
	buckets: make(map[string]*rate.Limiter),
	r:       10,  // 10 requests/second sustained
	burst:   30,  // allow short bursts up to 30
}

// getLimiter returns the existing limiter for key or creates one — O(1) amortized.
func (tb *tokenBucket) getLimiter(key string) *rate.Limiter {
	tb.mu.Lock()
	defer tb.mu.Unlock()
	if l, ok := tb.buckets[key]; ok {
		return l
	}
	l := rate.NewLimiter(tb.r, tb.burst)
	tb.buckets[key] = l
	return l
}

// RateLimit is a Gin middleware that enforces per-API-key token bucket rate limiting.
// Falls back to per-IP limiting when no API key is present (e.g. the webhook endpoint).
// Returns 429 with a Retry-After hint when the bucket is empty.
func RateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Use API key as the bucket identity; fall back to IP for unauthenticated routes.
		key := c.GetHeader("X-API-KEY")
		if key == "" {
			key = "ip:" + c.ClientIP()
		}

		limiter := globalBucket.getLimiter(key)
		if !limiter.Allow() {
			keyType := "api_key"
			if key[:3] == "ip:" {
				keyType = "ip"
			}
			observability.RateLimitHitsTotal.WithLabelValues(keyType).Inc()
			c.Header("Retry-After", "1")
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded — slow down and retry",
			})
			c.Abort()
			return
		}
		c.Next()
	}
}
