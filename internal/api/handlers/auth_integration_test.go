//go:build integration

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/services"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type chanSender struct{ codes chan string }

func (s chanSender) SendOTP(_ context.Context, _, code string) error { s.codes <- code; return nil }

func postJSON(t *testing.T, h gin.HandlerFunc, body any, setup func(*gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	if setup != nil {
		setup(c)
	}
	h(c)
	return w
}

// seedWorkerLogin — an org, an employee and an active users row for phone.
func seedWorkerLogin(t *testing.T, phone string) (orgID, employeeID string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	employeeID = "EMP-" + uuid.New().String()[:8]
	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		orgID, "otp test org").Error)
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Create(&models.Employee{
			ID: employeeID, OrganizationID: orgID, Name: "Tunde",
			Email:         models.EncryptedString("tunde-" + uuid.New().String()[:8] + "@example.com"),
			AccountNumber: "0123456789", BankCode: "058", IsActive: true,
		}).Error
	}))
	require.NoError(t, models.DB.Create(&models.User{EmployeeID: employeeID, OrgID: orgID, Phone: phone}).Error)
	return orgID, employeeID
}

func newTestWorkerAuth(t *testing.T) (*WorkerAuthHandler, chan string) {
	t.Helper()
	mr := miniredis.RunT(t)
	codes := make(chan string, 4)
	otp := services.NewOTPService(redis.NewClient(&redis.Options{Addr: mr.Addr()}), chanSender{codes})
	return NewWorkerAuthHandler(repository.NewUserRepository(models.DB), repository.NewEmployeeRepository(models.DB), otp), codes
}

func awaitCode(t *testing.T, codes chan string) string {
	t.Helper()
	select {
	case c := <-codes:
		return c
	case <-time.After(2 * time.Second):
		t.Fatal("no otp sent")
		return ""
	}
}

// Regression for the pre-launch audit's critical finding: WorkerLogin
// discarded the OTP (`_ = req.OTP`), so anyone who knew a worker's phone
// number got a token for their account.
func TestWorkerLogin_WrongOTPIsRejected(t *testing.T) {
	skipIfNoDB(t)
	h, codes := newTestWorkerAuth(t)
	phone := testPhone()
	seedWorkerLogin(t, phone)

	require.Equal(t, http.StatusAccepted, postJSON(t, h.RequestOTP, map[string]string{"phone": phone}, nil).Code)
	code := awaitCode(t, codes)
	wrong := "000000"
	if code == wrong {
		wrong = "111111"
	}

	w := postJSON(t, h.WorkerLogin, map[string]string{"phone": phone, "otp": wrong}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.NotContains(t, w.Body.String(), "token")
}

func TestWorkerLogin_NoOTPRequestedIsRejected(t *testing.T) {
	skipIfNoDB(t)
	h, _ := newTestWorkerAuth(t)
	phone := testPhone()
	seedWorkerLogin(t, phone)

	w := postJSON(t, h.WorkerLogin, map[string]string{"phone": phone, "otp": "123456"}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestWorkerLogin_CorrectOTPIssuesWorkerToken(t *testing.T) {
	skipIfNoDB(t)
	h, codes := newTestWorkerAuth(t)
	phone := testPhone()
	_, employeeID := seedWorkerLogin(t, phone)

	require.Equal(t, http.StatusAccepted, postJSON(t, h.RequestOTP, map[string]string{"phone": phone}, nil).Code)
	code := awaitCode(t, codes)

	w := postJSON(t, h.WorkerLogin, map[string]string{"phone": phone, "otp": code}, nil)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp map[string]string
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp["token"])
	assert.Equal(t, employeeID, resp["employee_id"])

	replay := postJSON(t, h.WorkerLogin, map[string]string{"phone": phone, "otp": code}, nil)
	assert.Equal(t, http.StatusUnauthorized, replay.Code, "a code must not log in twice")
}

// Unknown and suspended phones must look exactly like a real one from the
// outside: same 202, same 401 at login — no account-state oracle.
func TestWorkerAuth_UnknownAndSuspendedPhonesAreIndistinguishable(t *testing.T) {
	skipIfNoDB(t)
	h, codes := newTestWorkerAuth(t)

	suspended := testPhone()
	seedWorkerLogin(t, suspended)
	require.NoError(t, models.DB.Model(&models.User{}).Where("phone = ?", suspended).Update("is_active", false).Error)

	for _, phone := range []string{testPhone(), suspended} {
		req := postJSON(t, h.RequestOTP, map[string]string{"phone": phone}, nil)
		assert.Equal(t, http.StatusAccepted, req.Code)
		login := postJSON(t, h.WorkerLogin, map[string]string{"phone": phone, "otp": "123456"}, nil)
		assert.Equal(t, http.StatusUnauthorized, login.Code)
		assert.JSONEq(t, `{"error":"invalid credentials"}`, login.Body.String())
	}
	select {
	case <-codes:
		t.Fatal("no code may be sent to an unknown or suspended phone")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestWorkerAuth_NoSMSProviderFailsClosed(t *testing.T) {
	skipIfNoDB(t)
	h := NewWorkerAuthHandler(repository.NewUserRepository(models.DB), repository.NewEmployeeRepository(models.DB),
		services.NewOTPService(nil, nil))
	phone := testPhone()
	seedWorkerLogin(t, phone)

	assert.Equal(t, http.StatusServiceUnavailable, postJSON(t, h.RequestOTP, map[string]string{"phone": phone}, nil).Code)
	assert.Equal(t, http.StatusServiceUnavailable,
		postJSON(t, h.WorkerLogin, map[string]string{"phone": phone, "otp": "123456"}, nil).Code)
}

func TestTerminate_RevokesWorkerLogin(t *testing.T) {
	skipIfNoDB(t)
	phone := testPhone()
	orgID, employeeID := seedWorkerLogin(t, phone)

	_, err := services.NewEmployeeTerminationService().Terminate(context.Background(), orgID, employeeID, "resigned", "127.0.0.1")
	require.NoError(t, err)

	var u models.User
	require.NoError(t, models.DB.First(&u, "employee_id = ?", employeeID).Error)
	assert.False(t, u.IsActive)
}

// --- employer auth ---

func seedEmployerOrg(t *testing.T, password string, active bool) string {
	t.Helper()
	org := models.Organization{Name: "login test org"}
	require.NoError(t, org.SetPassword(password))
	require.NoError(t, models.DB.Create(&org).Error)
	if !active {
		require.NoError(t, models.DB.Model(&org).Update("is_active", false).Error)
	}
	return org.ID
}

func TestEmployerLogin_DeactivatedOrgIsRefused(t *testing.T) {
	skipIfNoDB(t)
	h := &AuthHandler{OrgRepo: repository.NewOrganizationRepository(models.DB)}

	active := seedEmployerOrg(t, "correct horse", true)
	inactive := seedEmployerOrg(t, "correct horse", false)

	assert.Equal(t, http.StatusOK,
		postJSON(t, h.Login, map[string]string{"org_id": active, "password": "correct horse"}, nil).Code)
	w := postJSON(t, h.Login, map[string]string{"org_id": inactive, "password": "correct horse"}, nil)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.JSONEq(t, `{"error":"invalid credentials"}`, w.Body.String())
}

func TestEmployerLogin_UnknownOrgCostsABcryptCompare(t *testing.T) {
	skipIfNoDB(t)
	h := &AuthHandler{OrgRepo: repository.NewOrganizationRepository(models.DB)}

	start := time.Now()
	w := postJSON(t, h.Login, map[string]string{"org_id": "ORG-nope", "password": "x"}, nil)
	elapsed := time.Since(start)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	// bcrypt at cost 12 takes well over 50ms; a bare DB miss takes ~1ms.
	assert.Greater(t, elapsed, 50*time.Millisecond, "unknown org must not answer measurably faster than a wrong password")
}

func refreshWith(t *testing.T, h *AuthHandler, orgID string, authTime time.Time) *httptest.ResponseRecorder {
	return postJSON(t, h.RefreshToken, map[string]string{}, func(c *gin.Context) {
		c.Set(middleware.OrgIDKey, orgID)
		c.Set("role", "admin")
		c.Set(middleware.AuthTimeKey, authTime.Unix())
	})
}

func TestRefreshToken_Limits(t *testing.T) {
	skipIfNoDB(t)
	h := &AuthHandler{OrgRepo: repository.NewOrganizationRepository(models.DB)}
	active := seedEmployerOrg(t, "pw-123456", true)
	inactive := seedEmployerOrg(t, "pw-123456", false)

	assert.Equal(t, http.StatusOK, refreshWith(t, h, active, time.Now().Add(-time.Hour)).Code)
	assert.Equal(t, http.StatusUnauthorized, refreshWith(t, h, active, time.Now().Add(-25*time.Hour)).Code,
		"refresh must not extend a session past MaxSessionAge")
	assert.Equal(t, http.StatusUnauthorized, refreshWith(t, h, inactive, time.Now()).Code,
		"a deactivated org's tokens must not be renewable")
}
