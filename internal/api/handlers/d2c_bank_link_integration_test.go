//go:build integration

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

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
