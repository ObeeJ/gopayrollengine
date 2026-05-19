package middleware

import (
	"go-payroll-engine/internal/observability"
	"net/http"

	"github.com/gin-gonic/gin"
	lru "github.com/hashicorp/golang-lru/v2"
	"golang.org/x/time/rate"
)

// rateLimiterCapacity caps the in-memory bucket map so a rotating-IP attacker
// (one that cycles a new client IP per request) can't exhaust process memory.
// At ~150 bytes per rate.Limiter, 50k entries ≈ 7.5 MB — generous for legit
// traffic, hard cap against pathological clients. Least-recently-used buckets
// are evicted automatically; an evicted key gets a fresh full burst on its
// next request, which is a tolerable side effect for a value that was already
// idle long enough to fall out.
const rateLimiterCapacity = 50_000

// tokenBucket holds a rate.Limiter per identity key in an LRU cache.
// DSA: Token Bucket (per key, fixed burst refilling at a steady rate) backed
// by an LRU map. The LRU bounds memory in O(1) amortised lookup/insert.
type tokenBucket struct {
	buckets *lru.Cache[string, *rate.Limiter]
	r       rate.Limit // tokens per second
	burst   int        // max burst size
}

func newTokenBucket(capacity int, r rate.Limit, burst int) *tokenBucket {
	cache, err := lru.New[string, *rate.Limiter](capacity)
	if err != nil {
		// lru.New only errors on capacity <= 0, which we control at the call
		// site. A panic here is a programming error, not a runtime condition.
		panic("ratelimit: " + err.Error())
	}
	return &tokenBucket{buckets: cache, r: r, burst: burst}
}

var globalBucket = newTokenBucket(rateLimiterCapacity, 10, 30) // 10 RPS sustained, burst 30

// getLimiter returns the existing limiter for key or creates one — O(1).
// The LRU's internal lock makes this safe under concurrent access; a brief
// race where two callers both Add() yields one wasted limiter and one Get
// that returns the loser's value, which is harmless.
func (tb *tokenBucket) getLimiter(key string) *rate.Limiter {
	if l, ok := tb.buckets.Get(key); ok {
		return l
	}
	l := rate.NewLimiter(tb.r, tb.burst)
	tb.buckets.Add(key, l)
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
