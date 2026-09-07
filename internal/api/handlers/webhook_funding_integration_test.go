//go:build integration

package handlers

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedFundingAccount creates an org and its dedicated funding account —
// the row lookup_org_for_funding_account resolves a deposit webhook against.
func seedFundingAccount(t *testing.T) (orgID, accountReference string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	accountReference = "FUND-" + uuid.New().String()[:8]

	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		orgID, "funding webhook test org",
	).Error)

	require.NoError(t, models.DB.Create(&models.OrganizationFundingAccount{
		OrganizationID:   orgID,
		ProviderName:     "monnify",
		AccountReference: accountReference,
		AccountNumber:    "9000000000",
		AccountName:      "funding webhook test org",
		BankName:         "Test Bank",
		BankCode:         "999",
	}).Error)

	return orgID, accountReference
}

func signedFundingWebhookRequest(t *testing.T, accountReference, transactionReference string, amountNaira int64) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"eventType": "SUCCESSFUL_TRANSACTION",
		"eventData": map[string]interface{}{
			"transactionReference": transactionReference,
			"amountPaid":           amountNaira,
			"product": map[string]interface{}{
				"reference": accountReference,
			},
		},
	})
	require.NoError(t, err)

	mac := hmac.New(sha512.New, []byte(os.Getenv("MONNIFY_SECRET_KEY")))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/monnify", bytes.NewReader(body))
	req.Header.Set("monnify-signature", sig)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	return w, c
}

func TestFundingWebhook_CreditsPool(t *testing.T) {
	skipIfNoDB(t)
	orgID, accountRef := seedFundingAccount(t)
	txRef := "MFY-TXN-" + uuid.New().String()[:8]

	w, c := signedFundingWebhookRequest(t, accountRef, txRef, 5_000)
	(&WebhookHandler{}).HandleMonnifyWebhook(c)
	assert.Equal(t, http.StatusOK, w.Code)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		exposure, err := models.FundingExposure(tx, orgID, money.NGN)
		require.NoError(t, err)
		assert.Equal(t, money.Money{Minor: -5_000 * 100, Currency: money.NGN}, exposure,
			"a ₦5,000 deposit must credit the pool by exactly that much")
		return nil
	}))
}

func TestFundingWebhook_UnknownAccountReturns404(t *testing.T) {
	skipIfNoDB(t)
	os.Setenv("MONNIFY_SECRET_KEY", "test-webhook-secret")

	w, c := signedFundingWebhookRequest(t, "FUND-doesnotexist", "MFY-TXN-unknown", 1_000)
	(&WebhookHandler{}).HandleMonnifyWebhook(c)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// A second delivery of the same deposit notification must not credit the
// pool twice — RecordEmployerFunding's idempotency key is the ledger-level
// guard; this confirms the webhook path actually reaches it correctly.
func TestFundingWebhook_DuplicateDeliveryIsIdempotent(t *testing.T) {
	skipIfNoDB(t)
	orgID, accountRef := seedFundingAccount(t)
	txRef := "MFY-TXN-" + uuid.New().String()[:8]

	w1, c1 := signedFundingWebhookRequest(t, accountRef, txRef, 3_000)
	(&WebhookHandler{}).HandleMonnifyWebhook(c1)
	require.Equal(t, http.StatusOK, w1.Code)

	w2, c2 := signedFundingWebhookRequest(t, accountRef, txRef, 3_000)
	(&WebhookHandler{}).HandleMonnifyWebhook(c2)
	assert.Equal(t, http.StatusOK, w2.Code)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		exposure, err := models.FundingExposure(tx, orgID, money.NGN)
		require.NoError(t, err)
		assert.Equal(t, money.Money{Minor: -3_000 * 100, Currency: money.NGN}, exposure,
			"the duplicate delivery must not credit the pool a second time")
		return nil
	}))
}
