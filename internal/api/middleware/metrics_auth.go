package middleware

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// MetricsAuth guards /metrics with a bearer token (Prometheus sends it via its
// `authorization` scrape setting). The series carry per-tenant org_id labels,
// so an open endpoint tells anyone who can reach the API which organisations
// exist and how active each one is.
//
// An empty token means "no scrape credential configured": allowOpen decides
// whether that serves the endpoint anyway (development/test) or answers 404 as
// if it did not exist (everything else, so a forgotten variable fails closed).
func MetricsAuth(token string, allowOpen bool) gin.HandlerFunc {
	want := sha256.Sum256([]byte(token))
	return func(c *gin.Context) {
		if token == "" {
			if allowOpen {
				c.Next()
				return
			}
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		got := sha256.Sum256([]byte(bearer(c.GetHeader("Authorization"))))
		// Hashing first makes the comparison length-independent.
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			c.Header("WWW-Authenticate", `Bearer realm="metrics"`)
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}

func bearer(h string) string {
	const p = "Bearer "
	if len(h) > len(p) && strings.EqualFold(h[:len(p)], p) {
		return strings.TrimSpace(h[len(p):])
	}
	return ""
}
