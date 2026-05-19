package middleware

import (
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
	os.Setenv("JWT_SECRET", "unit-test-secret")
	InitJWT()
}

func newRouter(handler gin.HandlerFunc, target gin.HandlerFunc) *gin.Engine {
	r := gin.New()
	r.GET("/probe", handler, target)
	return r
}

func doGet(r *gin.Engine, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func okHandler(c *gin.Context) { c.Status(http.StatusOK) }

func TestJWTAuth_ValidEmployerToken(t *testing.T) {
	tok, err := IssueToken("ORG-1", "admin", time.Minute)
	require.NoError(t, err)

	r := newRouter(JWTAuth(), func(c *gin.Context) {
		assert.Equal(t, "ORG-1", c.GetString(OrgIDKey))
		assert.Equal(t, "admin", Role(c))
		assert.Equal(t, "", EmployeeID(c))
		c.Status(http.StatusOK)
	})
	w := doGet(r, tok)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestJWTAuth_ValidWorkerToken(t *testing.T) {
	tok, err := IssueWorkerToken("ORG-1", "EMP-1", time.Minute)
	require.NoError(t, err)

	r := newRouter(JWTAuth(), func(c *gin.Context) {
		assert.Equal(t, "EMP-1", EmployeeID(c))
		assert.Equal(t, "employee", Role(c))
		c.Status(http.StatusOK)
	})
	w := doGet(r, tok)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestJWTAuth_MissingHeader(t *testing.T) {
	r := newRouter(JWTAuth(), okHandler)
	w := doGet(r, "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestJWTAuth_MalformedHeader(t *testing.T) {
	r := newRouter(JWTAuth(), okHandler)
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	req.Header.Set("Authorization", "Token foo")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestJWTAuth_ExpiredToken(t *testing.T) {
	tok, err := IssueToken("ORG-1", "admin", -time.Minute)
	require.NoError(t, err)
	r := newRouter(JWTAuth(), okHandler)
	w := doGet(r, tok)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestJWTAuth_WrongSecret(t *testing.T) {
	claims := Claims{
		OrgID: "ORG-1",
		Role:  "admin",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
		},
	}
	bad, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("wrong-secret"))
	require.NoError(t, err)
	r := newRouter(JWTAuth(), okHandler)
	w := doGet(r, bad)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestJWTAuth_NoneAlgorithmRejected(t *testing.T) {
	// Classic algorithm-confusion attack: attacker swaps alg to "none".
	claims := Claims{
		OrgID: "ORG-1",
		Role:  "admin",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
		},
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)
	require.NoError(t, err)
	r := newRouter(JWTAuth(), okHandler)
	w := doGet(r, tok)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireRole(t *testing.T) {
	cases := []struct {
		role, required string
		want           int
	}{
		{"admin", "admin", http.StatusOK},
		{"viewer", "admin", http.StatusForbidden},
		{"", "admin", http.StatusForbidden},
	}
	for _, tc := range cases {
		r := gin.New()
		r.GET("/x",
			func(c *gin.Context) { c.Set("role", tc.role); c.Next() },
			RequireRole(tc.required),
			okHandler,
		)
		req := httptest.NewRequest(http.MethodGet, "/x", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, tc.want, w.Code, "role=%q required=%q", tc.role, tc.required)
	}
}

func TestRequireWorker(t *testing.T) {
	cases := []struct {
		role, empID string
		want        int
	}{
		{"employee", "EMP-1", http.StatusOK},
		{"employee", "", http.StatusForbidden},
		{"admin", "EMP-1", http.StatusForbidden},
		{"", "", http.StatusForbidden},
	}
	for _, tc := range cases {
		r := gin.New()
		r.GET("/w",
			func(c *gin.Context) {
				c.Set("role", tc.role)
				c.Set("employee_id", tc.empID)
				c.Next()
			},
			RequireWorker(),
			okHandler,
		)
		req := httptest.NewRequest(http.MethodGet, "/w", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, tc.want, w.Code, "role=%q emp=%q", tc.role, tc.empID)
	}
}

func TestRequireEmployer(t *testing.T) {
	cases := []struct {
		role string
		want int
	}{
		{"admin", http.StatusOK},
		{"viewer", http.StatusOK},
		{"compliance", http.StatusOK},
		{"employee", http.StatusForbidden},
	}
	for _, tc := range cases {
		r := gin.New()
		r.GET("/e",
			func(c *gin.Context) { c.Set("role", tc.role); c.Next() },
			RequireEmployer(),
			okHandler,
		)
		req := httptest.NewRequest(http.MethodGet, "/e", nil)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		assert.Equal(t, tc.want, w.Code, "role=%q", tc.role)
	}
}
