package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestIdempotencyRouter wires an Idempotency middleware to a router whose
// only route is a counter-incrementing handler. The caller can inspect the
// counter to assert how many times the handler actually executed — the
// load-bearing property of an idempotency lock is that two identical
// requests in flight run the handler exactly once between them.
func newTestIdempotencyRouter(t *testing.T) (*gin.Engine, *atomic.Int32) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mr := miniredis.RunT(t) // RunT registers cleanup; caller doesn't need the handle
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	r := gin.New()
	var hits atomic.Int32
	r.POST("/pay", Idempotency(rdb), func(c *gin.Context) {
		hits.Add(1)
		c.JSON(http.StatusCreated, gin.H{"hits": hits.Load()})
	})
	return r, &hits
}

func doPost(t *testing.T, r *gin.Engine, body string, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/pay", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestIdempotency_MissingKeyRejected enforces the contract: mutating
// requests must carry an Idempotency-Key. Allowing missing keys would
// quietly disable the protection.
func TestIdempotency_MissingKeyRejected(t *testing.T) {
	r, hits := newTestIdempotencyRouter(t)
	w := doPost(t, r, `{}`, "")
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, int32(0), hits.Load(), "handler must not run when the key is missing")
}

// TestIdempotency_CacheHitReplay locks in the core promise: a second request
// with the same key replays the cached response and does not re-execute the
// handler — exactly one debit per (idempotency-key, account) pair.
func TestIdempotency_CacheHitReplay(t *testing.T) {
	r, hits := newTestIdempotencyRouter(t)

	w1 := doPost(t, r, `{}`, "key-abc")
	assert.Equal(t, http.StatusCreated, w1.Code)
	assert.Equal(t, int32(1), hits.Load())

	w2 := doPost(t, r, `{}`, "key-abc")
	assert.Equal(t, http.StatusCreated, w2.Code)
	assert.Equal(t, "true", w2.Header().Get("Idempotent-Replayed"))
	assert.Equal(t, int32(1), hits.Load(), "handler must not run again on cache hit")

	// Replayed body must equal original body, not a fresh handler invocation.
	var got map[string]int
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &got))
	assert.Equal(t, 1, got["hits"])
}

// TestIdempotency_InFlightSETNXBlocksConcurrentDupes is the Wave 2 #6a fix.
// N goroutines fire identical POSTs simultaneously; exactly one must reach
// the handler. The others must see the in-flight sentinel (or lose the
// SETNX race) and receive 409 Conflict.
func TestIdempotency_InFlightSETNXBlocksConcurrentDupes(t *testing.T) {
	// A handler that blocks until released — simulates a slow downstream call
	// (e.g. Monnify) so multiple concurrent requests overlap on the in-flight
	// window before the first one finishes and caches.
	gin.SetMode(gin.TestMode)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	release := make(chan struct{})
	var hits atomic.Int32

	r := gin.New()
	r.POST("/pay", Idempotency(rdb), func(c *gin.Context) {
		hits.Add(1)
		<-release // hold the handler open so concurrent dupes overlap with us
		c.JSON(http.StatusCreated, gin.H{"hits": hits.Load()})
	})

	const concurrent = 20
	codes := make([]int, concurrent)
	var wg sync.WaitGroup
	wg.Add(concurrent)
	start := make(chan struct{})
	for i := 0; i < concurrent; i++ {
		go func(idx int) {
			defer wg.Done()
			<-start
			w := doPost(t, r, `{}`, "same-key")
			codes[idx] = w.Code
		}(i)
	}
	close(start)

	// Give all 20 requests time to reach the middleware and contend on the
	// SETNX. 50 ms is generous on a developer laptop; CI may need more but
	// the test only cares about the win/lose distribution, not timing.
	time.Sleep(50 * time.Millisecond)
	close(release) // unblock the one winner
	wg.Wait()

	var winners, conflicts, other int
	for _, code := range codes {
		switch code {
		case http.StatusCreated:
			winners++
		case http.StatusConflict:
			conflicts++
		default:
			other++
		}
	}
	t.Logf("concurrent=%d, winners=%d, conflicts=%d, other=%d", concurrent, winners, conflicts, other)

	assert.Equal(t, int32(1), hits.Load(), "handler must run exactly once across N concurrent dupes")
	assert.Equal(t, 1, winners, "exactly one request must produce 201")
	assert.Equal(t, concurrent-1, conflicts, "every other request must get 409 Conflict")
	assert.Equal(t, 0, other, "no other status codes expected")
}

// TestIdempotency_FailedResponseReleasesLock proves the cleanup path: a
// non-2xx response must DEL the sentinel so the client's next retry isn't
// blocked until inflightTTL expires. Without this, a transient downstream
// failure would stick clients in 409 jail for a minute.
func TestIdempotency_FailedResponseReleasesLock(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	var hits atomic.Int32
	r := gin.New()
	r.POST("/pay", Idempotency(rdb), func(c *gin.Context) {
		hits.Add(1)
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "downstream down"})
	})

	w1 := doPost(t, r, `{}`, "key-503")
	assert.Equal(t, http.StatusServiceUnavailable, w1.Code)
	assert.Equal(t, int32(1), hits.Load())

	// Retry must run the handler again — the failure released the lock.
	w2 := doPost(t, r, `{}`, "key-503")
	assert.Equal(t, http.StatusServiceUnavailable, w2.Code)
	assert.Equal(t, int32(2), hits.Load(), "non-2xx must release the lock so retries are not blocked")
}

// TestIdempotency_BodyDrainPreserved confirms the middleware drains and
// re-injects the request body so the wrapped handler can read it normally.
// Earlier versions consumed the body to compute the hash and left an empty
// reader for the handler.
func TestIdempotency_BodyDrainPreserved(t *testing.T) {
	gin.SetMode(gin.TestMode)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	r := gin.New()
	r.POST("/pay", Idempotency(rdb), func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		require.NoError(t, err)
		c.Data(http.StatusOK, "application/json", body)
	})

	w := doPost(t, r, `{"hello":"world"}`, "echo-key")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.JSONEq(t, `{"hello":"world"}`, w.Body.String())
}

// newScopedIdempotencyRouter — like newTestIdempotencyRouter, but the org
// comes from an X-Test-Org header (standing in for TenantMiddleware) and two
// routes share the middleware, so key scoping can be exercised.
func newScopedIdempotencyRouter(t *testing.T) (*gin.Engine, *atomic.Int32) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})

	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set(OrgIDKey, c.GetHeader("X-Test-Org")); c.Next() })
	var hits atomic.Int32
	handler := func(c *gin.Context) {
		n := hits.Add(1)
		c.JSON(http.StatusCreated, gin.H{"org": OrgID(c), "path": c.FullPath(), "hit": n})
	}
	r.POST("/pay", Idempotency(rdb), handler)
	r.POST("/other", Idempotency(rdb), handler)
	return r, &hits
}

func scopedPost(r *gin.Engine, path, org, key, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-Org", org)
	req.Header.Set("Idempotency-Key", key)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// Regression: the Redis key was "idempotency:"+key, global across tenants,
// so org B sending a key org A had used got org A's cached response back.
func TestIdempotency_KeyIsScopedPerOrg(t *testing.T) {
	r, hits := newScopedIdempotencyRouter(t)

	a := scopedPost(r, "/pay", "ORG-A", "shared-key", `{"period":"2026-09"}`)
	require.Equal(t, http.StatusCreated, a.Code)
	b := scopedPost(r, "/pay", "ORG-B", "shared-key", `{"period":"2026-09"}`)
	require.Equal(t, http.StatusCreated, b.Code)

	assert.Empty(t, b.Header().Get("Idempotent-Replayed"), "org B must not receive org A's replay")
	assert.Contains(t, b.Body.String(), `"org":"ORG-B"`)
	assert.Equal(t, int32(2), hits.Load())
}

func TestIdempotency_KeyIsScopedPerRoute(t *testing.T) {
	r, hits := newScopedIdempotencyRouter(t)

	require.Equal(t, http.StatusCreated, scopedPost(r, "/pay", "ORG-A", "k1", `{}`).Code)
	other := scopedPost(r, "/other", "ORG-A", "k1", `{}`)
	require.Equal(t, http.StatusCreated, other.Code)
	assert.Contains(t, other.Body.String(), `"path":"/other"`)
	assert.Equal(t, int32(2), hits.Load())
}

func TestIdempotency_SameKeyDifferentBodyIsRefused(t *testing.T) {
	r, hits := newScopedIdempotencyRouter(t)

	require.Equal(t, http.StatusCreated, scopedPost(r, "/pay", "ORG-A", "k2", `{"period":"2026-09"}`).Code)
	w := scopedPost(r, "/pay", "ORG-A", "k2", `{"period":"2026-10"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Equal(t, int32(1), hits.Load(), "a different payload under a reused key must not run or replay")

	same := scopedPost(r, "/pay", "ORG-A", "k2", `{"period":"2026-09"}`)
	assert.Equal(t, http.StatusCreated, same.Code)
	assert.Equal(t, "true", same.Header().Get("Idempotent-Replayed"))
}

func TestIdempotency_OverlongKeyRejected(t *testing.T) {
	r, hits := newScopedIdempotencyRouter(t)
	w := scopedPost(r, "/pay", "ORG-A", strings.Repeat("k", maxIdempotencyKeyLen+1), `{}`)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, int32(0), hits.Load())
}
