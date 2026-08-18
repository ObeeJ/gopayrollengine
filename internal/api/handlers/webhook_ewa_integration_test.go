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

// seedApprovedAdvance creates an org, employee, and one EWA advance already
// submitted to a provider — the exact state the webhook fires against.
func seedApprovedAdvance(t *testing.T, amountKobo money.Kobo) (orgID, advanceID string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	employeeID := "EMP-" + uuid.New().String()[:8]

	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		orgID, "ewa webhook test org",
	).Error)

	require.NoError(t, models.DB.Create(&models.Employee{
		ID:             employeeID,
		OrganizationID: orgID,
		Name:           "Ada",
		Email:          models.EncryptedString("ewa-webhook-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		Salary:         money.FromNaira(300_000),
		IsActive:       true,
	}).Error)

	advance := models.EWAAdvance{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		Period:         "webhook-" + uuid.New().String()[:8],
		AmountKobo:     amountKobo,
		Status:         models.AdvanceApproved,
		DependencyTier: models.TierHealthy,
	}
	require.NoError(t, models.DB.Create(&advance).Error)

	providerRef := "MNFY-" + advance.ID + "-" + uuid.New().String()[:8]
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		// Mirrors EWAService.postAdvanceLedger: RequestAdvance posts this entry
		// at approval time, and CancelAdvance's reversal assumes it exists —
		// without it, cancellation drives the receivable negative instead of to zero.
		receivable, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
		if err != nil {
			return err
		}
		cash, err := models.EnsureAccount(tx, orgID, "", models.AccountCashSettlement, money.NGN)
		if err != nil {
			return err
		}
		if _, err := models.PostTransaction(tx, models.PostingRequest{
			OrgID:          orgID,
			Kind:           "ewa_advance",
			Reference:      advance.ID,
			IdempotencyKey: "ewa_advance:" + advance.ID,
			Entries: []models.EntryInput{
				{AccountID: receivable.ID, Direction: models.Debit, Amount: money.NGNFromKobo(advance.AmountKobo)},
				{AccountID: cash.ID, Direction: models.Credit, Amount: money.NGNFromKobo(advance.AmountKobo)},
			},
		}); err != nil {
			return err
		}
		return models.MarkSubmittedToProvider(tx, advance.ID, "monnify", providerRef)
	}))

	return orgID, advance.ID
}

func signedEWAWebhookRequest(t *testing.T, advanceID, eventType string, amountKobo money.Kobo) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"eventType": eventType,
		"eventData": map[string]interface{}{
			"batchReference":       "irrelevant",
			"transactionReference": advanceID,
			"status":               "SUCCESS",
			"amount":               amountKobo.Naira(),
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

// This is the confirmation the whole disbursement pipeline exists for: a
// DISBURSEMENT_SUCCESSFUL webhook must move the advance from Approved (the
// state ProcessEWADisbursementTask leaves it in) to Disbursed, and nowhere
// else in this codebase performs that transition.
func TestEWAWebhook_DisbursementSuccessfulConfirms(t *testing.T) {
	skipIfNoDB(t)
	amount := money.FromNaira(5_000)
	orgID, advanceID := seedApprovedAdvance(t, amount)
	_ = orgID

	w, c := signedEWAWebhookRequest(t, advanceID, "DISBURSEMENT_SUCCESSFUL", amount)
	(&WebhookHandler{}).HandleMonnifyWebhook(c)
	assert.Equal(t, http.StatusOK, w.Code)

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", advanceID).Error)
	assert.Equal(t, models.AdvanceDisbursed, advance.Status)
	require.NotNil(t, advance.DisbursedAt)
}

// A provider that accepted the transfer and later reports failure must reverse
// the receivable exactly as the settlement-time cancellation path does — cash
// never actually left, so the worker owes nothing.
func TestEWAWebhook_DisbursementFailedCancelsAndReverses(t *testing.T) {
	skipIfNoDB(t)
	amount := money.FromNaira(5_000)
	orgID, advanceID := seedApprovedAdvance(t, amount)

	w, c := signedEWAWebhookRequest(t, advanceID, "DISBURSEMENT_FAILED", amount)
	(&WebhookHandler{}).HandleMonnifyWebhook(c)
	assert.Equal(t, http.StatusOK, w.Code)

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", advanceID).Error)
	assert.Equal(t, models.AdvanceCancelled, advance.Status)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		recv, err := models.EnsureAccount(tx, orgID, advance.EmployeeID, models.AccountAdvanceReceivable, money.NGN)
		require.NoError(t, err)
		bal, err := models.AccountBalance(tx, recv.ID)
		require.NoError(t, err)
		assert.Equal(t, money.Money{Currency: money.NGN}, bal,
			"the receivable must be fully reversed after a post-acceptance failure")
		return nil
	}))
}

// The amount check that already protects payroll settlement must protect EWA
// disbursement the same way: a webhook reporting a different amount than the
// advance is refused rather than silently confirming a mismatched settlement.
func TestEWAWebhook_AmountMismatchIsRejected(t *testing.T) {
	skipIfNoDB(t)
	orgID, advanceID := seedApprovedAdvance(t, money.FromNaira(5_000))
	_ = orgID

	w, c := signedEWAWebhookRequest(t, advanceID, "DISBURSEMENT_SUCCESSFUL", money.FromNaira(50_000))
	(&WebhookHandler{}).HandleMonnifyWebhook(c)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", advanceID).Error)
	assert.Equal(t, models.AdvanceApproved, advance.Status,
		"a mismatched amount must not be allowed to confirm disbursement")
}

// A second delivery of the same successful webhook (Monnify retries on
// anything but 200, and networks duplicate) must be a no-op, not a second
// confirmation attempt against an advance that already moved past Approved.
func TestEWAWebhook_DuplicateDeliveryIsIdempotent(t *testing.T) {
	skipIfNoDB(t)
	amount := money.FromNaira(5_000)
	_, advanceID := seedApprovedAdvance(t, amount)

	w1, c1 := signedEWAWebhookRequest(t, advanceID, "DISBURSEMENT_SUCCESSFUL", amount)
	(&WebhookHandler{}).HandleMonnifyWebhook(c1)
	require.Equal(t, http.StatusOK, w1.Code)

	w2, c2 := signedEWAWebhookRequest(t, advanceID, "DISBURSEMENT_SUCCESSFUL", amount)
	(&WebhookHandler{}).HandleMonnifyWebhook(c2)
	assert.Equal(t, http.StatusOK, w2.Code, "a duplicate delivery must still return 200, not error")

	var advance models.EWAAdvance
	require.NoError(t, models.DB.First(&advance, "id = ?", advanceID).Error)
	assert.Equal(t, models.AdvanceDisbursed, advance.Status)
}

// A webhook for a reference the platform has never heard of (wrong ID, or an
// advance that predates this deployment) must 404, not panic or 500 — the ID
// prefix routing this handler relies on ("EWA-" vs everything else) must not
// assume the row exists once it has decided which table to query.
func TestEWAWebhook_UnknownAdvanceReturns404(t *testing.T) {
	skipIfNoDB(t)
	os.Setenv("MONNIFY_SECRET_KEY", "test-webhook-secret")

	w, c := signedEWAWebhookRequest(t, "EWA-doesnotexist", "DISBURSEMENT_SUCCESSFUL", money.FromNaira(1_000))
	(&WebhookHandler{}).HandleMonnifyWebhook(c)
	assert.Equal(t, http.StatusNotFound, w.Code)
}
