package middleware

import (
	"crypto/hmac"
	"go-payroll-engine/internal/observability"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Claims — the payload embedded in every JWT.
// OrgID is set for employer tokens. EmployeeID is set for worker tokens.
// They are mutually exclusive — a token is either an employer or a worker, never both.
type Claims struct {
	OrgID      string `json:"org_id"`
	EmployeeID string `json:"employee_id"` // set only on worker tokens
	Role       string `json:"role"`        // "admin" | "viewer" | "compliance" | "employee"
	jwt.RegisteredClaims
}

// jwtSecret — loaded once at startup; rotate via env without redeploying code.
var jwtSecret []byte

// InitJWT — loads the signing secret or kills the process; unsigned tokens are not an option.
func InitJWT() {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		if os.Getenv("APP_ENV") == "production" {
			log.Fatal("FATAL: JWT_SECRET not set. Refusing to start in production.")
		}
		log.Println("WARNING: JWT_SECRET not set — using insecure dev secret.")
		secret = "dev-secret-change-me"
	}
	jwtSecret = []byte(secret)
}

// IssueToken — mints a signed JWT for an employer org.
func IssueToken(orgID, role string, ttl time.Duration) (string, error) {
	claims := Claims{
		OrgID: orgID,
		Role:  role,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret)
}

// IssueWorkerToken — mints a signed JWT for a worker (employee app user).
// EmployeeID is embedded so every query can be scoped to that specific worker.
func IssueWorkerToken(orgID, employeeID string, ttl time.Duration) (string, error) {
	claims := Claims{
		OrgID:      orgID,
		EmployeeID: employeeID,
		Role:       "employee",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret)
}

// JWTAuth — validates the Bearer token and injects org_id + role into context.
// Replaces the single shared API key with per-tenant identity — no more APP_ORG_ID env var.
func JWTAuth() gin.HandlerFunc {
	return func(c *gin.Context) {
		header := c.GetHeader("Authorization")
		if !strings.HasPrefix(header, "Bearer ") {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "missing or malformed Authorization header"})
			c.Abort()
			return
		}
		tokenStr := strings.TrimPrefix(header, "Bearer ")

		claims := &Claims{}
		token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
			// Reject tokens signed with anything other than HMAC — algorithm confusion attack prevention.
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		})

		if err != nil || !token.Valid {
			observability.AuthFailuresTotal.WithLabelValues("jwt_invalid").Inc()
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			c.Abort()
			return
		}

		// Inject claims into context — downstream handlers use OrgID(c), EmployeeID(c), Role(c).
		c.Set(OrgIDKey, claims.OrgID)
		c.Set("employee_id", claims.EmployeeID)
		c.Set("role", claims.Role)
		c.Next()
	}
}

// APIKeyAuth — kept for webhook-adjacent tooling and backward compat; JWT is preferred for humans.
func APIKeyAuth() gin.HandlerFunc {
	requiredKey := os.Getenv("APP_API_KEY")
	if requiredKey == "" {
		log.Fatal("FATAL: APP_API_KEY not set. Refusing to start.")
	}
	return func(c *gin.Context) {
		apiKey := c.GetHeader("X-API-KEY")
		if !hmac.Equal([]byte(apiKey), []byte(requiredKey)) {
			observability.AuthFailuresTotal.WithLabelValues("api_key_wrong").Inc()
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Unauthorized"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequireRole — RBAC gate; call after JWTAuth to restrict endpoints by role.
func RequireRole(role string) gin.HandlerFunc {
	return func(c *gin.Context) {
		r, _ := c.Get("role")
		if r != role {
			c.JSON(http.StatusForbidden, gin.H{"error": "insufficient permissions"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// Role — pulls the role out of context set by JWTAuth.
func Role(c *gin.Context) string {
	v, _ := c.Get("role")
	r, _ := v.(string)
	return r
}

// EmployeeID — pulls the employee_id from context; only set on worker tokens.
// Empty string means the caller is an employer, not a worker.
func EmployeeID(c *gin.Context) string {
	v, _ := c.Get("employee_id")
	id, _ := v.(string)
	return id
}

// RequireWorker — gate that only allows worker (employee) tokens through.
// Prevents employers from hitting worker-only endpoints.
func RequireWorker() gin.HandlerFunc {
	return func(c *gin.Context) {
		if Role(c) != "employee" {
			c.JSON(http.StatusForbidden, gin.H{"error": "this endpoint is for workers only"})
			c.Abort()
			return
		}
		if EmployeeID(c) == "" {
			c.JSON(http.StatusForbidden, gin.H{"error": "worker identity missing from token"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequireEmployer — gate that only allows employer (admin/viewer/compliance) tokens through.
// Prevents workers from hitting employer-only endpoints.
func RequireEmployer() gin.HandlerFunc {
	return func(c *gin.Context) {
		if Role(c) == "employee" {
			c.JSON(http.StatusForbidden, gin.H{"error": "this endpoint is for employers only"})
			c.Abort()
			return
		}
		c.Next()
	}
}
