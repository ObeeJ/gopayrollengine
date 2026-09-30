package middleware

import (
	"go-payroll-engine/internal/observability"
	"net/http"
	"strconv"
	"time"

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

var (
	globalBucket = newTokenBucket(rateLimiterCapacity, 10, 30) // 10 RPS sustained, burst 30
	// authBucket — credential, OTP and signup endpoints: burst 10, refilling
	// one every 6s (10/min). Separate from globalBucket so ordinary API
	// traffic can't be spent on it. Not tighter because Nigerian mobile
	// carriers put many subscribers behind one CGNAT address; OTP guessing
	// is bounded per phone by services.OTPService, not by this.
	authBucket = newTokenBucket(rateLimiterCapacity, rate.Every(6*time.Second), 10)
)

// getLimiter — returns or creates the limiter for key; safe under concurrent access.
func (tb *tokenBucket) getLimiter(key string) *rate.Limiter {
	if l, ok := tb.buckets.Get(key); ok {
		return l
	}
	l := rate.NewLimiter(tb.r, tb.burst)
	tb.buckets.Add(key, l)
	return l
}

// RateLimit — token bucket per client IP; 429 with Retry-After when empty.
//
// Keyed on c.ClientIP() only. It used to key on the X-API-KEY header when
// present, which is never validated, so sending a fresh random value per
// request gave every request its own full bucket. ClientIP() honours
// X-Forwarded-For only from proxies the engine trusts (see
// api.configureTrustedProxies), so that header can't be spoofed either.
func RateLimit() gin.HandlerFunc { return rateLimitWith(globalBucket, "ip", 1) }

// AuthRateLimit — the stricter per-IP limit for login, OTP and signup
// endpoints; stacks on top of RateLimit.
func AuthRateLimit() gin.HandlerFunc { return rateLimitWith(authBucket, "auth_ip", 6) }

func rateLimitWith(tb *tokenBucket, keyType string, retryAfterSeconds int) gin.HandlerFunc {
	retryAfter := strconv.Itoa(retryAfterSeconds)
	return func(c *gin.Context) {
		if !tb.getLimiter(c.ClientIP()).Allow() {
			observability.RateLimitHitsTotal.WithLabelValues(keyType).Inc()
			c.Header("Retry-After", retryAfter)
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": "rate limit exceeded — slow down and retry",
			})
			c.Abort()
			return
		}
		c.Next()
	}
}
