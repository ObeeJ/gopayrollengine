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
			// Cache hit — replay the original response, do not re-execute the handler.
			var resp cachedResponse
			if json.Unmarshal(cached, &resp) == nil {
				c.Header("Idempotent-Replayed", "true")
				c.Data(resp.Status, "application/json", resp.Body)
				c.Abort()
				return
			}
		}

		// Cache miss — wrap the writer to capture the response for caching.
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

		// Only cache successful responses — do not cache 4xx/5xx so clients can retry.
		if rec.status >= 200 && rec.status < 300 {
			resp := cachedResponse{Status: rec.status, Body: rec.body.Bytes()}
			if data, err := json.Marshal(resp); err == nil {
				// SET with TTL — O(1). Key expires automatically, no cleanup needed.
				rdb.Set(ctx, redisKey, data, idempotencyTTL)
			}
		}
	}
}
