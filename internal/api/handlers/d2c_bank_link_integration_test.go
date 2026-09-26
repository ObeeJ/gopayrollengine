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
	"go-payroll-engine/internal/integrations/banklink"
	"go-payroll-engine/internal/models"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedD2CBankLinkWorker creates a bare D2C org + employee — the minimum a
// worker-scoped bank-link request needs, without an existing bank link.
func seedD2CBankLinkWorker(t *testing.T) (orgID, employeeID string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	employeeID = "EMP-" + uuid.New().String()[:8]

	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, is_d2c, created_at, updated_at) VALUES (?, ?, TRUE, NOW(), NOW())",
		orgID, "d2c bank-link test org",
	).Error)
	require.NoError(t, models.DB.Create(&models.Employee{
		ID:             employeeID,
		OrganizationID: orgID,
		Name:           "Bank Link Worker",
		Email:          models.EncryptedString("d2c-banklink-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		IsActive:       true,
	}).Error)
	return orgID, employeeID
}

func d2cWorkerRequest(t *testing.T, path, orgID, employeeID string, body map[string]any) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, path, reader)
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set(middleware.OrgIDKey, orgID)
	c.Set("employee_id", employeeID)
	c.Set("role", "employee")
	return w, c
}

func TestD2CBankLink_InitiateReturnsSessionFromProvider(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CBankLinkWorker(t)

	h := NewD2CBankLinkHandler(banklink.NewMock())
	w, c := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/initiate", orgID, employeeID, nil)
	h.InitiateLink(c)

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	var resp struct {
		SessionToken string `json:"session_token"`
		RedirectURL  string `json:"redirect_url"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.SessionToken)
	assert.NotEmpty(t, resp.RedirectURL)
}

func TestD2CBankLink_CompletePersistsLinkAndConsent(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CBankLinkWorker(t)
	mock := banklink.NewMock()

	w, c := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/complete", orgID, employeeID,
		map[string]any{"callback_token": "any-token", "consent": true})
	NewD2CBankLinkHandler(mock).CompleteLink(c)

	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	var link models.D2CBankLink
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &link))
	assert.Equal(t, models.D2CBankLinkLinked, link.Status)
	assert.Equal(t, mock.Name(), link.Provider)
	assert.Nil(t, link.DebitMandateRef)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		var stored models.D2CBankLink
		if err := tx.First(&stored, "id = ?", link.ID).Error; err != nil {
			return err
		}
		assert.Equal(t, employeeID, stored.EmployeeID)
		return nil
	}))

	var consents []models.ConsentRecord
	require.NoError(t, models.DB.Where("employee_id = ? AND consent_type = ?", employeeID, "d2c_bank_link_read").Find(&consents).Error)
	require.Len(t, consents, 1)
	assert.True(t, consents[0].Granted)
}

func TestD2CBankLink_CompleteRejectsMissingConsent(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CBankLinkWorker(t)

	w, c := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/complete", orgID, employeeID,
		map[string]any{"callback_token": "any-token", "consent": false})
	NewD2CBankLinkHandler(banklink.NewMock()).CompleteLink(c)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestD2CBankLink_AuthorizeDebitRequiresExistingLink(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CBankLinkWorker(t)

	w, c := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/authorize-debit", orgID, employeeID,
		map[string]any{"consent": true})
	NewD2CBankLinkHandler(banklink.NewMock()).AuthorizeDebit(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestD2CBankLink_AuthorizeDebitSetsMandateAndConsent(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CBankLinkWorker(t)
	mock := banklink.NewMock()

	// Link first, exactly as a real flow would.
	w1, c1 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/complete", orgID, employeeID,
		map[string]any{"callback_token": "any-token", "consent": true})
	NewD2CBankLinkHandler(mock).CompleteLink(c1)
	require.Equal(t, http.StatusCreated, w1.Code, w1.Body.String())

	w2, c2 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/authorize-debit", orgID, employeeID,
		map[string]any{"consent": true})
	NewD2CBankLinkHandler(mock).AuthorizeDebit(c2)

	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())
	var resp struct {
		DebitMandateRef string `json:"debit_mandate_ref"`
	}
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.DebitMandateRef)

	var link models.D2CBankLink
	require.NoError(t, models.DB.Where("employee_id = ?", employeeID).First(&link).Error)
	require.NotNil(t, link.DebitMandateRef)
	assert.Equal(t, resp.DebitMandateRef, *link.DebitMandateRef)
	assert.NotNil(t, link.DebitAuthorizedAt)

	var consents []models.ConsentRecord
	require.NoError(t, models.DB.Where("employee_id = ? AND consent_type = ?", employeeID, "d2c_debit_mandate").Find(&consents).Error)
	require.Len(t, consents, 1)
	assert.True(t, consents[0].Granted)
}

func TestD2CBankLink_AuthorizeDebitRejectsMissingConsent(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CBankLinkWorker(t)
	mock := banklink.NewMock()

	w1, c1 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/complete", orgID, employeeID,
		map[string]any{"callback_token": "any-token", "consent": true})
	NewD2CBankLinkHandler(mock).CompleteLink(c1)
	require.Equal(t, http.StatusCreated, w1.Code, w1.Body.String())

	w2, c2 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/authorize-debit", orgID, employeeID,
		map[string]any{"consent": false})
	NewD2CBankLinkHandler(mock).AuthorizeDebit(c2)

	assert.Equal(t, http.StatusBadRequest, w2.Code)
}

// With no real provider configured — routes.go passes a nil d2cProvider
// through outside MOCK_MODE — every endpoint must answer with a clean 503
// instead of dereferencing a nil provider or, as it did before routes.go
// registered this group unconditionally, not existing at all (a bare 404
// with no explanation). None of these touch the DB, so no skipIfNoDB.
func TestD2CBankLink_AllEndpointsRefuseWithNoProvider(t *testing.T) {
	h := NewD2CBankLinkHandler(nil)

	w1, c1 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/initiate", "ORG-x", "EMP-x", nil)
	h.InitiateLink(c1)
	assert.Equal(t, http.StatusServiceUnavailable, w1.Code, w1.Body.String())

	w2, c2 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/complete", "ORG-x", "EMP-x",
		map[string]any{"callback_token": "any-token", "consent": true})
	h.CompleteLink(c2)
	assert.Equal(t, http.StatusServiceUnavailable, w2.Code, w2.Body.String())

	w3, c3 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/authorize-debit", "ORG-x", "EMP-x",
		map[string]any{"consent": true})
	h.AuthorizeDebit(c3)
	assert.Equal(t, http.StatusServiceUnavailable, w3.Code, w3.Body.String())
}

// Revoke withdraws the whole link: read access (Status -> Revoked) and,
// since a mandate was also outstanding, the debit-mandate consent too —
// both as separate ConsentRecord rows, matching how CompleteLink and
// AuthorizeDebit each recorded their own grant.
func TestD2CBankLink_RevokeWithdrawsLinkAndBothConsents(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CBankLinkWorker(t)
	mock := banklink.NewMock()

	w1, c1 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/complete", orgID, employeeID,
		map[string]any{"callback_token": "any-token", "consent": true})
	NewD2CBankLinkHandler(mock).CompleteLink(c1)
	require.Equal(t, http.StatusCreated, w1.Code, w1.Body.String())

	w2, c2 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/authorize-debit", orgID, employeeID,
		map[string]any{"consent": true})
	NewD2CBankLinkHandler(mock).AuthorizeDebit(c2)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())

	w3, c3 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/revoke", orgID, employeeID, nil)
	NewD2CBankLinkHandler(mock).Revoke(c3)
	require.Equal(t, http.StatusOK, w3.Code, w3.Body.String())

	var link models.D2CBankLink
	require.NoError(t, models.DB.Where("employee_id = ?", employeeID).First(&link).Error)
	assert.Equal(t, models.D2CBankLinkRevoked, link.Status)
	assert.NotNil(t, link.RevokedAt)

	var consents []models.ConsentRecord
	require.NoError(t, models.DB.Where("employee_id = ? AND granted = false", employeeID).
		Order("consent_type").Find(&consents).Error)
	require.Len(t, consents, 2, "revoking a link with a mandate must withdraw both consents")
	assert.Equal(t, models.ConsentTypeD2CBankLinkRead, consents[0].ConsentType)
	assert.Equal(t, models.ConsentTypeD2CDebitMandate, consents[1].ConsentType)
}

// No debit mandate was ever authorized — Revoke must still withdraw the
// read consent, and must not fabricate a debit-mandate withdrawal for a
// consent that was never granted in the first place.
func TestD2CBankLink_RevokeWithoutMandateOnlyWithdrawsReadConsent(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CBankLinkWorker(t)
	mock := banklink.NewMock()

	w1, c1 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/complete", orgID, employeeID,
		map[string]any{"callback_token": "any-token", "consent": true})
	NewD2CBankLinkHandler(mock).CompleteLink(c1)
	require.Equal(t, http.StatusCreated, w1.Code, w1.Body.String())

	w2, c2 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/revoke", orgID, employeeID, nil)
	NewD2CBankLinkHandler(mock).Revoke(c2)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())

	var consents []models.ConsentRecord
	require.NoError(t, models.DB.Where("employee_id = ? AND granted = false", employeeID).Find(&consents).Error)
	require.Len(t, consents, 1)
	assert.Equal(t, models.ConsentTypeD2CBankLinkRead, consents[0].ConsentType)
}

func TestD2CBankLink_RevokeReturns404WithNoLinkedAccount(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CBankLinkWorker(t)

	w, c := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/revoke", orgID, employeeID, nil)
	NewD2CBankLinkHandler(banklink.NewMock()).Revoke(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// Revoke must work even when the handler carries no provider — there's
// nothing to call out to (it only ever touches our own records), and a
// worker must always be able to withdraw consent, whether or not a live
// provider happens to be configured right now. Seeds the link directly
// since a nil provider can't run CompleteLink itself.
func TestD2CBankLink_RevokeWorksEvenWithNilProvider(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CBankLinkWorker(t)
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Create(&models.D2CBankLink{
			OrganizationID:     orgID,
			EmployeeID:         employeeID,
			Provider:           "mock",
			ProviderAccountRef: "mock-acct-nil-provider",
			LinkedAt:           time.Now(),
		}).Error
	}))

	w, c := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/revoke", orgID, employeeID, nil)
	NewD2CBankLinkHandler(nil).Revoke(c)

	assert.Equal(t, http.StatusOK, w.Code, w.Body.String())
}

// RevokeDebitMandate must leave read access untouched — Status stays
// Linked — since that consent is deliberately separate; only the mandate
// itself and its own consent are withdrawn.
func TestD2CBankLink_RevokeDebitMandateLeavesReadAccessIntact(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CBankLinkWorker(t)
	mock := banklink.NewMock()

	w1, c1 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/complete", orgID, employeeID,
		map[string]any{"callback_token": "any-token", "consent": true})
	NewD2CBankLinkHandler(mock).CompleteLink(c1)
	require.Equal(t, http.StatusCreated, w1.Code, w1.Body.String())

	w2, c2 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/authorize-debit", orgID, employeeID,
		map[string]any{"consent": true})
	NewD2CBankLinkHandler(mock).AuthorizeDebit(c2)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())

	w3, c3 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/revoke-debit-mandate", orgID, employeeID, nil)
	NewD2CBankLinkHandler(mock).RevokeDebitMandate(c3)
	require.Equal(t, http.StatusOK, w3.Code, w3.Body.String())

	var link models.D2CBankLink
	require.NoError(t, models.DB.Where("employee_id = ?", employeeID).First(&link).Error)
	assert.Equal(t, models.D2CBankLinkLinked, link.Status, "read access must survive a debit-mandate-only revoke")
	assert.Nil(t, link.DebitMandateRef)
	assert.Nil(t, link.DebitAuthorizedAt)

	var consents []models.ConsentRecord
	require.NoError(t, models.DB.Where("employee_id = ? AND granted = false", employeeID).Find(&consents).Error)
	require.Len(t, consents, 1)
	assert.Equal(t, models.ConsentTypeD2CDebitMandate, consents[0].ConsentType)
}

// No mandate was ever authorized — a no-op, not an error, and no
// withdrawal ConsentRecord for a consent that was never granted.
func TestD2CBankLink_RevokeDebitMandateNoOpWhenNoMandateExists(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CBankLinkWorker(t)
	mock := banklink.NewMock()

	w1, c1 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/complete", orgID, employeeID,
		map[string]any{"callback_token": "any-token", "consent": true})
	NewD2CBankLinkHandler(mock).CompleteLink(c1)
	require.Equal(t, http.StatusCreated, w1.Code, w1.Body.String())

	w2, c2 := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/revoke-debit-mandate", orgID, employeeID, nil)
	NewD2CBankLinkHandler(mock).RevokeDebitMandate(c2)
	require.Equal(t, http.StatusOK, w2.Code, w2.Body.String())

	var count int64
	require.NoError(t, models.DB.Model(&models.ConsentRecord{}).
		Where("employee_id = ? AND granted = false", employeeID).Count(&count).Error)
	assert.Zero(t, count)
}

func TestD2CBankLink_RevokeDebitMandateReturns404WithNoLinkedAccount(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CBankLinkWorker(t)

	w, c := d2cWorkerRequest(t, "/api/v1/worker/d2c/bank-link/revoke-debit-mandate", orgID, employeeID, nil)
	NewD2CBankLinkHandler(banklink.NewMock()).RevokeDebitMandate(c)

	assert.Equal(t, http.StatusNotFound, w.Code)
}
