package middleware

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// maxBodyBytes is the maximum allowed request body size (1 MB).
// Prevents memory exhaustion from oversized payloads — a common DoS vector.
const maxBodyBytes = 1 << 20 // 1 MB

// SecurityHeaders sets defensive HTTP headers on every response.
// These headers are required by OWASP and expected by any security audit.
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

// BodySizeLimit rejects requests whose body exceeds maxBodyBytes.
// Without this, a 500 MB JSON payload would be read entirely into memory
// before any handler logic runs — a trivial memory exhaustion attack.
func BodySizeLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBodyBytes)
		c.Next()
	}
}
