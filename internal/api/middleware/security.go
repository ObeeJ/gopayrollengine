package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// maxBodyBytes — request body cap (1 MB); oversized bodies are a cheap DoS vector.
const maxBodyBytes = 1 << 20 // 1 MB

// SecurityHeaders — sets the defensive HTTP headers OWASP and auditors expect.
func SecurityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Prevent browsers from MIME-sniffing the content type.
		c.Header("X-Content-Type-Options", "nosniff")
		// Deny framing to block clickjacking attacks.
		c.Header("X-Frame-Options", "DENY")
		// Force HTTPS for 1 year; include subdomains.
		c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		// Restrict what resources the browser can load — API only serves JSON.
		c.Header("Content-Security-Policy", "default-src 'none'")
		// Disable cross-origin resource sharing for API responses.
		c.Header("X-Permitted-Cross-Domain-Policies", "none")
		// Stop sending Referer header to third parties.
		c.Header("Referrer-Policy", "no-referrer")
		// Remove the server fingerprint header Gin sets by default.
		c.Header("Server", "")
		c.Next()
	}
}

// BodySizeLimit — caps the request body at maxBodyBytes before any handler reads it.
func BodySizeLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		c.Next()
	}
}
