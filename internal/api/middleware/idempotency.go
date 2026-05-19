package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// idempotencyTTL is how long a key + cached response is retained in Redis.
// 24 hours covers any reasonable client retry window for payment operations.
const idempotencyTTL = 24 * time.Hour

// inflightTTL bounds how long a SETNX in-flight marker can live before
// being reclaimed. Generous enough to outlast a slow handler (a payroll
// create that spawns an Asynq task plus N item inserts can take seconds);
// short enough that a crashed worker doesn't permanently lock the key.
const inflightTTL = 60 * time.Second

// inflightSentinel marks a key as in-progress between cache miss and
// successful completion. Replaced atomically by the real cached response
// when the handler finishes; remains until inflightTTL expires if the
// handler crashes.
const inflightSentinel = "__inflight__"

// cachedResponse is the structure stored in Redis for each idempotency key.
// Storing status + body lets us replay the exact original response on retries.
type cachedResponse struct {
	Status int    `json:"status"`
	Body   []byte `json:"body"`
}

// responseRecorder wraps gin.ResponseWriter to capture the response
// so we can cache it in Redis after the handler writes it.
type responseRecorder struct {
	gin.ResponseWriter
	body   *bytes.Buffer
	status int
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	r.body.Write(b)
	return r.ResponseWriter.Write(b)
}

func (r *responseRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

// Idempotency is a Gin middleware that enforces idempotency on mutating endpoints.
//
// DSA: Hash Map in Redis. Key = "idempotency:{Idempotency-Key}", Value = cached response.
// O(1) Redis GET — cache hit replays the original response immediately,
// replayed immediately — no DB touched, no double payment possible.
//
// Clients must send a unique "Idempotency-Key" header (UUID recommended).
// Missing key → 400. Duplicate key within TTL → cached response replayed with 200.
func Idempotency(rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := c.GetHeader("Idempotency-Key")
		if key == "" {
			c.JSON(http.StatusBadRequest, gin.H{
				"error": "Idempotency-Key header is required for mutating requests",
			})
			c.Abort()
			return
		}

		redisKey := "idempotency:" + key
		ctx := context.Background()

		// O(1) Redis GET — check if this key was already processed.
		cached, err := rdb.Get(ctx, redisKey).Bytes()
		if err == nil {
			// Distinguish the in-flight sentinel from a completed response. A
			// second identical request arriving while the first is still being
			// processed must NOT execute the handler again — that would double-
			// debit the payment. We reject it with 409; the client should retry
			// after a short backoff, by which time the original response will
			// have replaced the sentinel.
			if string(cached) == inflightSentinel {
				c.Header("Retry-After", "1")
				c.JSON(http.StatusConflict, gin.H{
					"error": "a request with this Idempotency-Key is still in flight; retry shortly",
				})
				c.Abort()
				return
			}
			// Cache hit — replay the original response, do not re-execute the handler.
			var resp cachedResponse
			if json.Unmarshal(cached, &resp) == nil {
				c.Header("Idempotent-Replayed", "true")
				c.Data(resp.Status, "application/json", resp.Body)
				c.Abort()
				return
			}
		}

		// Cache miss — claim the key with SETNX. Only the first caller wins;
		// concurrent duplicate requests see the sentinel above and back off.
		// Without this lock, two requests that both miss the cache could both
		// run the handler — two payroll batches created from one Idempotency-
		// Key, which is the exact failure mode the header is supposed to prevent.
		acquired, err := rdb.SetNX(ctx, redisKey, inflightSentinel, inflightTTL).Result()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "idempotency backend unavailable"})
			c.Abort()
			return
		}
		if !acquired {
			// Another concurrent request acquired the lock between our GET and SETNX.
			c.Header("Retry-After", "1")
			c.JSON(http.StatusConflict, gin.H{
				"error": "a request with this Idempotency-Key is still in flight; retry shortly",
			})
			c.Abort()
			return
		}

		// Wrap the writer to capture the response for caching.
		rec := &responseRecorder{
			ResponseWriter: c.Writer,
			body:           &bytes.Buffer{},
			status:         http.StatusOK,
		}
		c.Writer = rec

		// Drain and re-inject the request body so the handler can read it normally.
		bodyBytes, _ := io.ReadAll(c.Request.Body)
		c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))

		c.Next()

		// Only cache successful responses — do not cache 4xx/5xx so clients
		// can retry. On non-2xx, release the in-flight lock immediately so
		// the next attempt isn't blocked until inflightTTL expires.
		if rec.status >= 200 && rec.status < 300 {
			resp := cachedResponse{Status: rec.status, Body: rec.body.Bytes()}
			if data, err := json.Marshal(resp); err == nil {
				// Overwrites the sentinel with the real response, atomically
				// extending the TTL to the full retry window.
				rdb.Set(ctx, redisKey, data, idempotencyTTL)
			}
		} else {
			rdb.Del(ctx, redisKey)
		}
	}
}
