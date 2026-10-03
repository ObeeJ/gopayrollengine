package middleware

import (
	"crypto/hmac"
	"go-payroll-engine/internal/appenv"
	"go-payroll-engine/internal/observability"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// Claims — JWT payload; OrgID for employers, EmployeeID for workers, never both.
type Claims struct {
	OrgID      string `json:"org_id"`
	EmployeeID string `json:"employee_id"` // set only on worker tokens
	Role       string `json:"role"`        // "admin" | "viewer" | "compliance" | "employee"
	// UserID — the employer person behind the token (employer_users.id).
	// Empty on legacy org-password tokens and on worker tokens.
	UserID string `json:"user_id,omitempty"`
	// MustChangePassword — the holder logged in with a temporary password and
	// may do nothing but change it; see RequirePasswordChanged.
	MustChangePassword bool `json:"must_change_password,omitempty"`
	// AuthTime — when the holder last actually proved their credentials
	// (unix seconds). Carried unchanged through refreshes so a session has
	// a hard ceiling; see MaxSessionAge.
	AuthTime int64 `json:"auth_time,omitempty"`
	jwt.RegisteredClaims
}

// jwtSecret — loaded once at startup; rotate via env without redeploying code.
var jwtSecret []byte

// InitJWT — loads the signing secret or kills the process; unsigned tokens are not an option.
func InitJWT() {
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		if !appenv.AllowsInsecureDefaults() {
			log.Fatal("FATAL: JWT_SECRET not set. Only development/test may fall back to a dev secret.")
		}
		log.Println("WARNING: JWT_SECRET not set — using insecure dev secret.")
		secret = "dev-secret-change-me"
	}
	jwtSecret = []byte(secret)
}

// MaxSessionAge — how long refreshes may extend a session past its login.
// Without a ceiling, a stolen token could be refreshed forever.
const MaxSessionAge = 24 * time.Hour

// AuthTimeKey — gin context key for the token's AuthTime.
const AuthTimeKey = "auth_time"

// IssueToken — mints a signed JWT for an employer org at login.
func IssueToken(orgID, role string, ttl time.Duration) (string, error) {
	return IssueRefreshedToken(orgID, role, time.Now(), ttl)
}

// IssueRefreshedToken — mints an employer JWT preserving the original login time.
func IssueRefreshedToken(orgID, role string, authTime time.Time, ttl time.Duration) (string, error) {
	return IssueUserToken(orgID, "", role, false, authTime, ttl)
}

// IssueUserToken — mints an employer JWT for a named person (userID empty for
// the legacy org-level login).
func IssueUserToken(orgID, userID, role string, mustChange bool, authTime time.Time, ttl time.Duration) (string, error) {
	claims := Claims{
		OrgID:              orgID,
		UserID:             userID,
		MustChangePassword: mustChange,
		Role:               role,
		AuthTime:           authTime.Unix(),
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(ttl)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(jwtSecret)
}

// IssueWorkerToken — mints a worker JWT with embedded employee_id for per-worker scoping.
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
			// Reject non-HMAC signatures — algorithm-confusion defence.
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return jwtSecret, nil
		}, jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}), jwt.WithExpirationRequired())

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
		c.Set(AuthTimeKey, claims.AuthTime)
		c.Set(UserIDKey, claims.UserID)
		c.Set(MustChangeKey, claims.MustChangePassword)
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

// AuthTime — when the token holder last logged in; zero if the token predates the claim.
func AuthTime(c *gin.Context) time.Time {
	v, _ := c.Get(AuthTimeKey)
	if t, ok := v.(int64); ok && t > 0 {
		return time.Unix(t, 0)
	}
	return time.Time{}
}

// EmployeeID — worker's employee_id from ctx; empty means caller is an employer.
func EmployeeID(c *gin.Context) string {
	v, _ := c.Get("employee_id")
	id, _ := v.(string)
	return id
}

// RequireWorker — gate that only lets worker tokens through.
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

// RequireActiveWorker — rejects a worker token whose login has since been
// deactivated (termination, suspension). Worker tokens live 8h; without this
// check, terminating someone left them in the worker app until it expired.
// Runs after RequireWorker. isActive failing is treated as a denial.
func RequireActiveWorker(isActive func(employeeID string) (bool, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		ok, err := isActive(EmployeeID(c))
		if err != nil || !ok {
			observability.AuthFailuresTotal.WithLabelValues("worker_inactive").Inc()
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequireEmployer — gate that only lets employer tokens through.
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

const (
	// UserIDKey / MustChangeKey — gin context keys for the per-person claims.
	UserIDKey     = "user_id"
	MustChangeKey = "must_change_password"
)

// UserID — the employer person's id; empty for legacy org-level tokens.
func UserID(c *gin.Context) string {
	v, _ := c.Get(UserIDKey)
	id, _ := v.(string)
	return id
}

// ActorName — who to put in the audit trail: the person when known, else the
// role (legacy org-level logins).
func ActorName(c *gin.Context) string {
	if id := UserID(c); id != "" {
		return id
	}
	return Role(c)
}

// RequirePasswordChanged — a session that began with a temporary password may
// only reach the password-change endpoint (and refresh); everything else
// answers 403 until the holder sets their own.
func RequirePasswordChanged() gin.HandlerFunc {
	return func(c *gin.Context) {
		if must, _ := c.Get(MustChangeKey); must == true {
			c.JSON(http.StatusForbidden, gin.H{"error": "password change required", "code": "password_change_required"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequireActiveEmployerUser — rejects a token whose person has been
// deactivated since it was issued (an 8h token otherwise outlives removal).
// Legacy org-level tokens (no user id) pass through.
func RequireActiveEmployerUser(isActive func(orgID, userID string) (bool, error)) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := UserID(c)
		if id == "" {
			c.Next()
			return
		}
		ok, err := isActive(OrgID(c), id)
		if err != nil || !ok {
			observability.AuthFailuresTotal.WithLabelValues("employer_user_inactive").Inc()
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid or expired token"})
			c.Abort()
			return
		}
		c.Next()
	}
}

// MustChange — whether the session is limited to changing the password.
func MustChange(c *gin.Context) bool {
	v, _ := c.Get(MustChangeKey)
	b, _ := v.(bool)
	return b
}
