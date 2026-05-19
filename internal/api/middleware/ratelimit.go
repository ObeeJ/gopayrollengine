package middleware

import (
	"go-payroll-engine/internal/observability"
	"net/http"

	"github.com/gin-gonic/gin"
	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/time/rate"
)

// rateLimiterCapacity — LRU cap so rotating-IP attackers can't OOM the process; ~7.5 MB at 50k entries.
const rateLimiterCapacity = 50_000

// tokenBucket — per-identity rate.Limiter behind an LRU; O(1) lookup, bounded memory.
type tokenBucket struct {
	buckets *lru.Cache[string, *rate.Limiter]
	r       rate.Limit // tokens per second
	burst   int        // max burst size
}

func newTokenBucket(capacity int, r rate.Limit, burst int) *tokenBucket {
	cache, err := lru.New[string, *rate.Limiter](capacity)
	if err != nil {
		// lru.New only errors on capacity <= 0 — a programming bug, not runtime.
		panic("ratelimit: " + err.Error())
	}
	return &tokenBucket{buckets: cache, r: r, burst: burst}
}

var globalBucket = newTokenBucket(rateLimiterCapacity, 10, 30) // 10 RPS sustained, burst 30

// getLimiter — returns or creates the limiter for key; safe under concurrent access.
func (tb *tokenBucket) getLimiter(key string) *rate.Limiter {
	if l, ok := tb.buckets.Get(key); ok {
		return l
	}
	l := rate.NewLimiter(tb.r, tb.burst)
	tb.buckets.Add(key, l)
	return l
}

// RateLimit — token-bucket per API key, falling back to per-IP; 429 with Retry-After when empty.
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
