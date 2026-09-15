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

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedD2CPendingCollection creates a D2C org, employee, Disbursed advance
// with its ledger already posted, and one pending D2CDebitCollection
// attempt — the exact state a debit provider's webhook fires against.
func seedD2CPendingCollection(t *testing.T, amountKobo money.Kobo) *models.D2CDebitCollection {
	t.Helper()
	orgID := "ORG-" + uuid.New().String()[:8]
	employeeID := "EMP-" + uuid.New().String()[:8]

	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, is_d2c, created_at, updated_at) VALUES (?, ?, TRUE, NOW(), NOW())",
		orgID, "d2c webhook test org",
	).Error)

	require.NoError(t, models.DB.Create(&models.Employee{
		ID:             employeeID,
		OrganizationID: orgID,
		Name:           "Chidi",
		Email:          models.EncryptedString("d2c-webhook-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		IsActive:       true,
	}).Error)

	advance := models.EWAAdvance{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		Period:         "d2c-webhook-" + uuid.New().String()[:8],
		AmountKobo:     amountKobo,
		Status:         models.AdvanceDisbursed,
		DependencyTier: models.TierHealthy,
	}
	require.NoError(t, models.DB.Create(&advance).Error)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		receivable, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
		if err != nil {
			return err
		}
		cash, err := models.EnsureAccount(tx, orgID, "", models.AccountCashSettlement, money.NGN)
		if err != nil {
			return err
		}
		_, err = models.PostTransaction(tx, models.PostingRequest{
			OrgID:          orgID,
			Kind:           "ewa_advance",
			Reference:      advance.ID,
			IdempotencyKey: "ewa_advance:" + advance.ID,
			Entries: []models.EntryInput{
				{AccountID: receivable.ID, Direction: models.Debit, Amount: money.NGNFromKobo(amountKobo)},
				{AccountID: cash.ID, Direction: models.Credit, Amount: money.NGNFromKobo(amountKobo)},
			},
		})
		return err
	}))

	providerRef := "mock-debit-" + uuid.New().String()[:8]
	c := models.D2CDebitCollection{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		AdvanceID:      advance.ID,
		AmountKobo:     amountKobo,
		AttemptNumber:  1,
		ScheduledFor:   time.Now(),
		Status:         models.D2CCollectionPending,
		Provider:       "mock",
		ProviderRef:    &providerRef,
	}
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Create(&c).Error
	}))

	return &c
}

func d2cDebitWebhookRequest(t *testing.T, providerRef, status, reason string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"provider_reference": providerRef,
		"status":             status,
		"reason":             reason,
	})
	require.NoError(t, err)

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/d2c-debit-collection", bytes.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	return w, c
}

func TestD2CDebitWebhook_SuccessfulConfirmsCollectionAndSettlesAdvance(t *testing.T) {
	skipIfNoDB(t)
	collection := seedD2CPendingCollection(t, money.FromNaira(10_000))

	w, c := d2cDebitWebhookRequest(t, *collection.ProviderRef, "successful", "")
	(&WebhookHandler{}).HandleD2CDebitWebhook(c)
	assert.Equal(t, http.StatusOK, w.Code)

	var reloaded models.D2CDebitCollection
	require.NoError(t, models.DB.First(&reloaded, "id = ?", collection.ID).Error)
	assert.Equal(t, models.D2CCollectionSuccessful, reloaded.Status)

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", collection.AdvanceID).Error)
	assert.Equal(t, models.AdvanceSettled, advance.Status)
}

func TestD2CDebitWebhook_FailedMarksCollectionFailedAndLeavesAdvanceDisbursed(t *testing.T) {
	skipIfNoDB(t)
	collection := seedD2CPendingCollection(t, money.FromNaira(10_000))

	w, c := d2cDebitWebhookRequest(t, *collection.ProviderRef, "failed", "insufficient_funds")
	(&WebhookHandler{}).HandleD2CDebitWebhook(c)
	assert.Equal(t, http.StatusOK, w.Code)

	var reloaded models.D2CDebitCollection
	require.NoError(t, models.DB.First(&reloaded, "id = ?", collection.ID).Error)
	assert.Equal(t, models.D2CCollectionFailed, reloaded.Status)

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", collection.AdvanceID).Error)
	assert.Equal(t, models.AdvanceDisbursed, advance.Status, "a single failure must leave room for retry")
}

func TestD2CDebitWebhook_UnknownReferenceReturns404(t *testing.T) {
	skipIfNoDB(t)
	w, c := d2cDebitWebhookRequest(t, "no-such-reference-"+uuid.New().String()[:8], "successful", "")
	(&WebhookHandler{}).HandleD2CDebitWebhook(c)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestD2CDebitWebhook_InvalidStatusReturns400(t *testing.T) {
	skipIfNoDB(t)
	collection := seedD2CPendingCollection(t, money.FromNaira(10_000))

	w, c := d2cDebitWebhookRequest(t, *collection.ProviderRef, "made_up_status", "")
	(&WebhookHandler{}).HandleD2CDebitWebhook(c)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// A duplicate delivery of the same event must be a no-op, not a second
// ledger posting or a crash on an already-resolved collection.
func TestD2CDebitWebhook_DuplicateDeliveryIsIdempotent(t *testing.T) {
	skipIfNoDB(t)
	collection := seedD2CPendingCollection(t, money.FromNaira(10_000))

	w1, c1 := d2cDebitWebhookRequest(t, *collection.ProviderRef, "successful", "")
	(&WebhookHandler{}).HandleD2CDebitWebhook(c1)
	assert.Equal(t, http.StatusOK, w1.Code)

	w2, c2 := d2cDebitWebhookRequest(t, *collection.ProviderRef, "successful", "")
	(&WebhookHandler{}).HandleD2CDebitWebhook(c2)
	assert.Equal(t, http.StatusOK, w2.Code)

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", collection.AdvanceID).Error)
	assert.Equal(t, money.FromNaira(10_000), advance.RecoveredKobo, "a duplicate delivery must not double-recover the advance")
}
