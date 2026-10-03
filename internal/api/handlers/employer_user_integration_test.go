//go:build integration

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"go-payroll-engine/internal/api/middleware"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/services"
	"go-payroll-engine/internal/testutil"
	"go-payroll-engine/pkg/money"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The router wires the same middleware chain as routes.go (JWT, employer gate,
// active-person check, forced password change, admin gate) so these tests cover
// the behaviour a client sees, not just the service.
func identityRouter() *gin.Engine {
	middleware.InitJWT()
	users := services.NewEmployerUserService()
	auth := &AuthHandler{OrgRepo: repository.NewOrganizationRepository(models.DB), Users: users}
	h := &EmployerUserHandler{Users: users}
	r := gin.New()
	r.POST("/auth/login", auth.Login)
	r.POST("/auth/password", middleware.JWTAuth(), middleware.RequireEmployer(),
		middleware.RequireActiveEmployerUser(users.IsActive), auth.ChangePassword)
	r.POST("/auth/refresh", middleware.JWTAuth(), middleware.RequireEmployer(), auth.RefreshToken)
	e := r.Group("/")
	e.Use(middleware.JWTAuth(), middleware.RequireEmployer(),
		middleware.RequireActiveEmployerUser(users.IsActive), middleware.RequirePasswordChanged())
	u := e.Group("/users", middleware.RequireRole("admin"))
	u.POST("/", h.Create)
	u.GET("/", h.List)
	u.PATCH("/:id", h.Update)
	u.POST("/:id/reset-password", h.ResetPassword)
	e.GET("/ping", func(c *gin.Context) { c.JSON(200, gin.H{"actor": middleware.ActorName(c)}) })
	return r
}

func doJSON(t *testing.T, r *gin.Engine, method, path, token, body string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

// onboard creates an org + first admin the way the create-org CLI does and
// returns the org, the admin's email and temporary password.
func onboard(t *testing.T) (orgID, email, temp string) {
	t.Helper()
	email = "boss-" + uuid.New().String()[:8] + "@example.com"
	org, _, temp, err := services.NewEmployerUserService().CreateOrganization(
		context.Background(), "Onboarded Ltd", money.NGN, email)
	require.NoError(t, err)
	return org.ID, email, temp
}

func login(t *testing.T, r *gin.Engine, email, pw string) (int, map[string]any) {
	t.Helper()
	b, _ := json.Marshal(map[string]string{"email": email, "password": pw})
	return doJSON(t, r, http.MethodPost, "/auth/login", "", string(b))
}

// firstLoginAndChange takes a person from temporary password to a full session.
func firstLoginAndChange(t *testing.T, r *gin.Engine, email, temp string) string {
	t.Helper()
	code, out := login(t, r, email, temp)
	require.Equal(t, http.StatusOK, code)
	b, _ := json.Marshal(map[string]string{"current_password": temp, "new_password": strongPW})
	code, out = doJSON(t, r, http.MethodPost, "/auth/password", out["token"].(string), string(b))
	require.Equal(t, http.StatusOK, code, "%v", out)
	return out["token"].(string)
}

const strongPW = "correct-horse-battery-1"

func TestEmployerIdentity_OnboardingAndForcedPasswordChange(t *testing.T) {
	skipIfNoDB(t)
	r := identityRouter()
	orgID, email, temp := onboard(t)

	// An onboarded org has no shared password: the legacy login must not work.
	b, _ := json.Marshal(map[string]string{"org_id": orgID, "password": ""})
	code, _ := doJSON(t, r, http.MethodPost, "/auth/login", "", string(b))
	assert.Equal(t, http.StatusBadRequest, code)

	code, out := login(t, r, email, temp)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, out["must_change_password"])
	assert.Equal(t, orgID, out["org_id"])
	temporaryToken := out["token"].(string)

	// A temporary-password session can do nothing else…
	code, out = doJSON(t, r, http.MethodGet, "/users/", temporaryToken, "")
	assert.Equal(t, http.StatusForbidden, code)
	assert.Equal(t, "password_change_required", out["code"])

	// …weak passwords are refused…
	weak, _ := json.Marshal(map[string]string{"current_password": temp, "new_password": "short"})
	code, _ = doJSON(t, r, http.MethodPost, "/auth/password", temporaryToken, string(weak))
	assert.Equal(t, http.StatusBadRequest, code)

	// …a wrong current password is refused…
	wrong, _ := json.Marshal(map[string]string{"current_password": "not-the-password", "new_password": strongPW})
	code, _ = doJSON(t, r, http.MethodPost, "/auth/password", temporaryToken, string(wrong))
	assert.Equal(t, http.StatusUnauthorized, code)

	// …and the right change unlocks the session with a fresh token.
	token := firstLoginAndChange(t, r, email, temp)
	code, _ = doJSON(t, r, http.MethodGet, "/users/", token, "")
	assert.Equal(t, http.StatusOK, code)

	code, _ = login(t, r, email, temp)
	assert.Equal(t, http.StatusUnauthorized, code, "the temporary password stops working once changed")
	code, _ = login(t, r, email, strongPW)
	assert.Equal(t, http.StatusOK, code)
}

func TestEmployerIdentity_LoginFailuresAreIndistinguishable(t *testing.T) {
	skipIfNoDB(t)
	r := identityRouter()
	_, email, _ := onboard(t)

	c1, o1 := login(t, r, email, "wrong-password-123")
	c2, o2 := login(t, r, "nobody-"+uuid.New().String()[:6]+"@example.com", "wrong-password-123")
	assert.Equal(t, http.StatusUnauthorized, c1)
	assert.Equal(t, c1, c2)
	assert.Equal(t, o1, o2, "unknown email and wrong password must read the same")
}

func TestEmployerIdentity_PersonalAuditTrailAndRoles(t *testing.T) {
	skipIfNoDB(t)
	r := identityRouter()
	orgID, adminEmail, temp := onboard(t)
	adminToken := firstLoginAndChange(t, r, adminEmail, temp)

	code, out := doJSON(t, r, http.MethodPost, "/users/", adminToken,
		`{"email":"Viewer-`+uuid.New().String()[:6]+`@Example.com","name":"Ada","role":"viewer"}`)
	require.Equal(t, http.StatusCreated, code, "%v", out)
	viewer := out["user"].(map[string]any)
	viewerTemp := out["temporary_password"].(string)
	assert.NotEmpty(t, viewerTemp)
	assert.NotContains(t, viewer, "password_hash")

	// The creation is attributed to the admin as a person, not to "admin".
	var audits []models.AuditEvent
	require.NoError(t, models.DB.Where("organization_id = ? AND action = ?", orgID, "user_created").Find(&audits).Error)
	var creator string
	for _, a := range audits {
		if a.EntityID == viewer["id"] {
			creator = a.ActorKey
		}
	}
	assert.Contains(t, creator, "EUS-", "audit actor must be the person's id")

	// A viewer logs in (after changing the temporary password) and cannot manage users.
	viewerToken := firstLoginAndChange(t, r, viewer["email"].(string), viewerTemp)
	code, _ = doJSON(t, r, http.MethodGet, "/users/", viewerToken, "")
	assert.Equal(t, http.StatusForbidden, code)
	code, out = doJSON(t, r, http.MethodGet, "/ping", viewerToken, "")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, viewer["id"], out["actor"])

	// Duplicate email (any case) is a conflict, an invalid role a 400.
	code, _ = doJSON(t, r, http.MethodPost, "/users/", adminToken,
		`{"email":"`+viewer["email"].(string)+`","role":"viewer"}`)
	assert.Equal(t, http.StatusConflict, code)
	code, _ = doJSON(t, r, http.MethodPost, "/users/", adminToken, `{"email":"x@example.com","role":"owner"}`)
	assert.Equal(t, http.StatusBadRequest, code)
}

func TestEmployerIdentity_DeactivationRevokesLiveTokens(t *testing.T) {
	skipIfNoDB(t)
	r := identityRouter()
	_, adminEmail, temp := onboard(t)
	adminToken := firstLoginAndChange(t, r, adminEmail, temp)
	_, out := doJSON(t, r, http.MethodPost, "/users/", adminToken, `{"email":"leaver-`+uuid.New().String()[:6]+`@example.com","role":"admin"}`)
	leaver := out["user"].(map[string]any)
	leaverToken := firstLoginAndChange(t, r, leaver["email"].(string), out["temporary_password"].(string))

	code, _ := doJSON(t, r, http.MethodGet, "/users/", leaverToken, "")
	require.Equal(t, http.StatusOK, code)

	code, _ = doJSON(t, r, http.MethodPatch, "/users/"+leaver["id"].(string), adminToken, `{"is_active":false}`)
	require.Equal(t, http.StatusOK, code)

	code, _ = doJSON(t, r, http.MethodGet, "/users/", leaverToken, "")
	assert.Equal(t, http.StatusUnauthorized, code, "an 8h token must not outlive the person's removal")
	code, _ = doJSON(t, r, http.MethodPost, "/auth/refresh", leaverToken, "")
	assert.Equal(t, http.StatusUnauthorized, code)
	code, _ = login(t, r, leaver["email"].(string), strongPW)
	assert.Equal(t, http.StatusUnauthorized, code)
}

func TestEmployerIdentity_ResetPasswordForcesChangeAndKillsOldPassword(t *testing.T) {
	skipIfNoDB(t)
	r := identityRouter()
	_, adminEmail, temp := onboard(t)
	adminToken := firstLoginAndChange(t, r, adminEmail, temp)
	_, out := doJSON(t, r, http.MethodPost, "/users/", adminToken, `{"email":"forgot-`+uuid.New().String()[:6]+`@example.com","role":"viewer"}`)
	u := out["user"].(map[string]any)
	firstLoginAndChange(t, r, u["email"].(string), out["temporary_password"].(string))

	code, out := doJSON(t, r, http.MethodPost, "/users/"+u["id"].(string)+"/reset-password", adminToken, "")
	require.Equal(t, http.StatusOK, code)
	newTemp := out["temporary_password"].(string)

	code, _ = login(t, r, u["email"].(string), strongPW)
	assert.Equal(t, http.StatusUnauthorized, code, "the forgotten password no longer works")
	code, out = login(t, r, u["email"].(string), newTemp)
	require.Equal(t, http.StatusOK, code)
	assert.Equal(t, true, out["must_change_password"])

	code, _ = doJSON(t, r, http.MethodPost, "/users/USR-nope/reset-password", adminToken, "")
	assert.Equal(t, http.StatusNotFound, code)
}

func TestEmployerIdentity_NeverLeavesOrgWithoutAdmin(t *testing.T) {
	skipIfNoDB(t)
	svc := services.NewEmployerUserService()
	ctx := context.Background()
	orgID, _, _ := onboard(t)
	users, err := svc.List(ctx, orgID)
	require.NoError(t, err)
	require.Len(t, users, 1)
	sole := users[0]
	act := services.Actor{Name: "test"}
	viewerRole, off := models.EmployerRoleViewer, false

	_, err = svc.Update(ctx, orgID, sole.ID, services.EmployerUserUpdate{Role: &viewerRole}, act)
	assert.ErrorIs(t, err, services.ErrLastAdmin)
	_, err = svc.Update(ctx, orgID, sole.ID, services.EmployerUserUpdate{IsActive: &off}, act)
	assert.ErrorIs(t, err, services.ErrLastAdmin)

	// With a second admin, two simultaneous demotions: exactly one may win.
	second, _, err := svc.Create(ctx, orgID, "second-"+uuid.New().String()[:6]+"@example.com", "", models.EmployerRoleAdmin, act)
	require.NoError(t, err)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, id := range []string{sole.ID, second.ID} {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			_, errs[i] = svc.Update(ctx, orgID, id, services.EmployerUserUpdate{Role: &viewerRole}, act)
		}(i, id)
	}
	wg.Wait()
	wins := 0
	for _, e := range errs {
		if e == nil {
			wins++
		} else {
			assert.ErrorIs(t, e, services.ErrLastAdmin)
		}
	}
	assert.Equal(t, 1, wins, "concurrent demotions must not remove the last admin")
	var admins int64
	require.NoError(t, models.DB.Model(&models.EmployerUser{}).
		Where("organization_id = ? AND role = 'admin' AND is_active", orgID).Count(&admins).Error)
	assert.EqualValues(t, 1, admins)
}

func TestEmployerIdentity_TenantIsolationUnderProductionRole(t *testing.T) {
	skipIfNoDB(t)
	orgA, emailA, tempA := onboard(t)
	orgB, emailB, tempB := onboard(t)
	_ = orgA
	svc := services.NewEmployerUserService()
	bUsers, err := svc.List(context.Background(), orgB)
	require.NoError(t, err)
	require.Len(t, bUsers, 1)

	testutil.UseAppRoleDB(t)
	r := identityRouter()

	// Login works under RLS (the SECURITY DEFINER lookup), for both orgs.
	tokenA := firstLoginAndChange(t, r, emailA, tempA)
	firstLoginAndChange(t, r, emailB, tempB)

	// A's admin cannot see, change or reset B's people.
	code, out := doJSON(t, r, http.MethodGet, "/users/", tokenA, "")
	require.Equal(t, http.StatusOK, code)
	assert.Len(t, out["users"], 1, "only A's own person is visible")
	code, _ = doJSON(t, r, http.MethodPatch, "/users/"+bUsers[0].ID, tokenA, `{"is_active":false}`)
	assert.Equal(t, http.StatusNotFound, code)
	code, _ = doJSON(t, r, http.MethodPost, "/users/"+bUsers[0].ID+"/reset-password", tokenA, "")
	assert.Equal(t, http.StatusNotFound, code)
}

func TestEmployerIdentity_OnboardingFailureLeavesNoOrphanOrg(t *testing.T) {
	skipIfNoDB(t)
	_, email, _ := onboard(t)
	var before int64
	require.NoError(t, models.DB.Model(&models.Organization{}).Count(&before).Error)

	_, _, _, err := services.NewEmployerUserService().CreateOrganization(
		context.Background(), "Dup Ltd", money.NGN, email) // email already taken
	assert.ErrorIs(t, err, services.ErrEmployerUserExists)

	var after int64
	require.NoError(t, models.DB.Model(&models.Organization{}).Count(&after).Error)
	assert.Equal(t, before, after, "a failed onboarding must not leave an org nobody can log in to")
}

func TestEmployerIdentity_LegacyOrgLoginStillWorksButHasNoPersonalPassword(t *testing.T) {
	skipIfNoDB(t)
	r := identityRouter()
	orgID := seedOrgWithCurrency(t, money.NGN)
	org := models.Organization{}
	require.NoError(t, org.SetPassword("legacy-shared-password"))
	require.NoError(t, models.DB.Exec("UPDATE organizations SET password_hash = ? WHERE id = ?", org.PasswordHash, orgID).Error)

	body, _ := json.Marshal(map[string]string{"org_id": orgID, "password": "legacy-shared-password"})
	code, out := doJSON(t, r, http.MethodPost, "/auth/login", "", string(body))
	require.Equal(t, http.StatusOK, code)
	token := out["token"].(string)

	code, out = doJSON(t, r, http.MethodGet, "/ping", token, "")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "admin", out["actor"], "legacy tokens keep attributing to the role")

	pw, _ := json.Marshal(map[string]string{"current_password": "legacy-shared-password", "new_password": strongPW})
	code, _ = doJSON(t, r, http.MethodPost, "/auth/password", token, string(pw))
	assert.Equal(t, http.StatusForbidden, code)
}
