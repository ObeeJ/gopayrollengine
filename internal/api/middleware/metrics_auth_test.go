package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
)

func metricsStatus(token string, allowOpen bool, authHeader string) int {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/metrics", MetricsAuth(token, allowOpen), func(c *gin.Context) { c.String(200, "series") })
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w.Code
}

func TestMetricsAuth(t *testing.T) {
	assert.Equal(t, 200, metricsStatus("s3cret", false, "Bearer s3cret"))
	assert.Equal(t, 200, metricsStatus("s3cret", false, "bearer s3cret"), "scheme is case-insensitive")
	assert.Equal(t, 401, metricsStatus("s3cret", false, ""))
	assert.Equal(t, 401, metricsStatus("s3cret", false, "Bearer wrong"))
	assert.Equal(t, 401, metricsStatus("s3cret", false, "Bearer s3cre"))
	assert.Equal(t, 401, metricsStatus("s3cret", false, "s3cret"), "token without the Bearer scheme")
	assert.Equal(t, 401, metricsStatus("s3cret", true, ""), "allowOpen only applies when no token is configured")
}

func TestMetricsAuth_NoTokenConfigured(t *testing.T) {
	assert.Equal(t, 200, metricsStatus("", true, ""), "development: open")
	assert.Equal(t, 404, metricsStatus("", false, ""), "production without a token: closed, as if absent")
	assert.Equal(t, 404, metricsStatus("", false, "Bearer "), "an empty bearer must not match an empty token")
}
