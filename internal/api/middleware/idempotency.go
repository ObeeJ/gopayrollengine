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

// idempotencyTTL — how long a key + cached response sticks around in Redis.
const idempotencyTTL = 24 * time.Hour

// inflightTTL — long enough to outlast a slow handler, short enough to free a crashed one.
const inflightTTL = 60 * time.Second

// inflightSentinel — placeholder value while the handler is running.
const inflightSentinel = "__inflight__"

// cachedResponse — what we replay on retry: original status + body.
type cachedResponse struct {
	Status int    `json:"status"`
	Body   []byte `json:"body"`
}

// responseRecorder — wraps gin.ResponseWriter to capture the response for caching.
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

// Idempotency — Gin middleware that replays cached responses for duplicate Idempotency-Key headers.
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
			// In-flight sentinel — refuse with 409 to avoid double-debit; client retries.
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

		// Cache miss — SETNX claims the key so concurrent duplicates back off.
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

		// Cache only 2xx; on errors release the lock so the next retry isn't blocked.
		if rec.status >= 200 && rec.status < 300 {
			resp := cachedResponse{Status: rec.status, Body: rec.body.Bytes()}
			if data, err := json.Marshal(resp); err == nil {
				// Overwrite sentinel with the real response, extending TTL to the full retry window.
				rdb.Set(ctx, redisKey, data, idempotencyTTL)
			}
		} else {
			rdb.Del(ctx, redisKey)
		}
	}
}
