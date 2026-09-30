package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newLimitedEngine(t *testing.T, mw gin.HandlerFunc) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Production posture: trust no proxy unless one is configured.
	require.NoError(t, r.SetTrustedProxies(nil))
	r.GET("/x", mw, func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

// uniqueIP isolates each test's bucket from its siblings' (buckets are package-global).
func uniqueIP() string {
	n := testCounter()
	return fmt.Sprintf("10.%d.%d.%d:1234", (n>>16)&0xff, (n>>8)&0xff, n&0xff)
}

func hit(r *gin.Engine, remoteAddr string, headers map[string]string) int {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = remoteAddr
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestRateLimit_UnderBurstAllowed(t *testing.T) {
	r := newLimitedEngine(t, RateLimit())
	ip := uniqueIP()
	for i := 0; i < 10; i++ {
		assert.Equal(t, http.StatusOK, hit(r, ip, nil), "request %d should pass", i)
	}
}

func TestRateLimit_OverBurstReturns429(t *testing.T) {
	r := newLimitedEngine(t, RateLimit())
	ip := uniqueIP()
	var got429 bool
	// Burst=30; firing 60 in a tight loop must exhaust the bucket.
	for i := 0; i < 60; i++ {
		if hit(r, ip, nil) == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	assert.True(t, got429, "expected at least one 429 after exhausting burst")
}

func TestRateLimit_PerIPIsolation(t *testing.T) {
	r := newLimitedEngine(t, RateLimit())
	a, b := uniqueIP(), uniqueIP()
	for i := 0; i < 60; i++ {
		hit(r, a, nil)
	}
	assert.Equal(t, http.StatusOK, hit(r, b, nil))
}

// Regression: the bucket used to be keyed on the unvalidated X-API-KEY
// header, so a fresh value per request bypassed the limit entirely.
func TestRateLimit_RotatingAPIKeyHeaderDoesNotBypass(t *testing.T) {
	r := newLimitedEngine(t, RateLimit())
	ip := uniqueIP()
	var got429 bool
	for i := 0; i < 60; i++ {
		if hit(r, ip, map[string]string{"X-API-KEY": fmt.Sprintf("junk-%d", i)}) == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	assert.True(t, got429)
}

// With no trusted proxies, a spoofed X-Forwarded-For must not mint a new bucket.
func TestRateLimit_SpoofedForwardedForDoesNotBypass(t *testing.T) {
	r := newLimitedEngine(t, RateLimit())
	ip := uniqueIP()
	var got429 bool
	for i := 0; i < 60; i++ {
		if hit(r, ip, map[string]string{"X-Forwarded-For": fmt.Sprintf("203.0.113.%d", i)}) == http.StatusTooManyRequests {
			got429 = true
			break
		}
	}
	assert.True(t, got429)
}

func TestAuthRateLimit_IsStricter(t *testing.T) {
	r := newLimitedEngine(t, AuthRateLimit())
	ip := uniqueIP()
	for i := 0; i < 10; i++ {
		assert.Equal(t, http.StatusOK, hit(r, ip, nil), "attempt %d within burst", i)
	}
	assert.Equal(t, http.StatusTooManyRequests, hit(r, ip, nil))
}

var counter int

func testCounter() int { counter++; return counter }
