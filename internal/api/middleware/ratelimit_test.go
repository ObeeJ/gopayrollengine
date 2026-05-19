package middleware

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func TestRateLimit_UnderBurstAllowed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", RateLimit(), func(c *gin.Context) { c.Status(http.StatusOK) })

	// Unique key isolates this test from any sibling test's bucket state.
	apiKey := "test-key-under-" + fmt.Sprint(testCounter())
	for i := 0; i < 10; i++ {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("X-API-KEY", apiKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, http.StatusOK, w.Code, "request %d should pass", i)
	}
}

func TestRateLimit_OverBurstReturns429(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", RateLimit(), func(c *gin.Context) { c.Status(http.StatusOK) })

	apiKey := "test-key-over-" + fmt.Sprint(testCounter())
	var got429 bool
	// Burst=30; firing 60 in a tight loop must exhaust the bucket.
	for i := 0; i < 60; i++ {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("X-API-KEY", apiKey)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code == http.StatusTooManyRequests {
			got429 = true
			assert.Equal(t, "1", w.Header().Get("Retry-After"))
			break
		}
	}
	assert.True(t, got429, "expected at least one 429 after exhausting burst")
}

func TestRateLimit_PerKeyIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/x", RateLimit(), func(c *gin.Context) { c.Status(http.StatusOK) })

	keyA := "iso-A-" + fmt.Sprint(testCounter())
	keyB := "iso-B-" + fmt.Sprint(testCounter())

	// Drain bucket A.
	for i := 0; i < 60; i++ {
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		req.Header.Set("X-API-KEY", keyA)
		r.ServeHTTP(httptest.NewRecorder(), req)
	}

	// B should still be fresh.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("X-API-KEY", keyB)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

var counter int

func testCounter() int { counter++; return counter }
