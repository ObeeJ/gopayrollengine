//go:build integration

package handlers

import (
	"context"
	"net/http"
	"testing"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedSubmittedGrant — a grant the worker has already submitted, with the
// ledger entry IssueHardshipGrant posts.
func seedSubmittedGrant(t *testing.T, amount money.Kobo) (orgID, grantID string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	employeeID := "EMP-" + uuid.New().String()[:8]
	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		orgID, "grant webhook test org").Error)
	require.NoError(t, models.DB.Create(&models.Employee{
		ID: employeeID, OrganizationID: orgID, Name: "Ife",
		Email:         models.EncryptedString("gw-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber: "0123456789", BankCode: "058", IsActive: true,
	}).Error)

	g := models.EWAHardshipGrant{OrganizationID: orgID, EmployeeID: employeeID, AmountKobo: amount,
		Reason: "flood damage", ApprovedByIP: "127.0.0.1"}
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		if err := tx.Create(&g).Error; err != nil {
			return err
		}
		cash, err := models.EnsureAccount(tx, orgID, "", models.AccountCashSettlement, money.NGN)
		if err != nil {
			return err
		}
		expense, err := models.EnsureAccount(tx, orgID, "", models.AccountHardshipGrantExpense, money.NGN)
		if err != nil {
			return err
		}
		if _, err := models.PostTransaction(tx, models.PostingRequest{
			OrgID: orgID, Kind: "hardship_grant", Reference: g.ID, IdempotencyKey: "hardship_grant:" + g.ID,
			Entries: []models.EntryInput{
				{AccountID: expense.ID, Direction: models.Debit, Amount: money.NGNFromKobo(amount)},
				{AccountID: cash.ID, Direction: models.Credit, Amount: money.NGNFromKobo(amount)},
			},
		}); err != nil {
			return err
		}
		return models.MarkGrantSubmitted(tx, g.ID, "monnify", "MFY-"+g.ID)
	}))
	return orgID, g.ID
}

func grantState(t *testing.T, orgID, grantID string) (models.EWAHardshipGrant, money.Money) {
	t.Helper()
	var g models.EWAHardshipGrant
	var bal money.Money
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		if err := tx.First(&g, "id = ?", grantID).Error; err != nil {
			return err
		}
		acct, err := models.EnsureAccount(tx, orgID, "", models.AccountHardshipGrantExpense, money.NGN)
		if err != nil {
			return err
		}
		bal, err = models.AccountBalance(tx, acct.ID)
		return err
	}))
	return g, bal
}

func TestGrantWebhook_SuccessMarksDisbursed(t *testing.T) {
	skipIfNoDB(t)
	amount := money.FromNaira(20_000)
	orgID, grantID := seedSubmittedGrant(t, amount)

	w, c := signedEWAWebhookRequest(t, grantID, "DISBURSEMENT_SUCCESSFUL", amount)
	(&WebhookHandler{}).HandleMonnifyWebhook(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	g, expense := grantState(t, orgID, grantID)
	assert.Equal(t, models.GrantDisbursed, g.Status)
	assert.NotNil(t, g.DisbursedAt)
	assert.Equal(t, amount, money.Kobo(expense.Minor), "a paid grant stays booked as expense")

	// Redelivery is a no-op.
	w, c = signedEWAWebhookRequest(t, grantID, "DISBURSEMENT_FAILED", amount)
	(&WebhookHandler{}).HandleMonnifyWebhook(c)
	assert.Equal(t, http.StatusOK, w.Code)
	g, _ = grantState(t, orgID, grantID)
	assert.Equal(t, models.GrantDisbursed, g.Status, "a late failure must not undo a confirmed payout")
}

func TestGrantWebhook_FailureReversesLedgerOnce(t *testing.T) {
	skipIfNoDB(t)
	amount := money.FromNaira(20_000)
	orgID, grantID := seedSubmittedGrant(t, amount)

	for i := 0; i < 2; i++ { // second delivery must not reverse twice
		w, c := signedEWAWebhookRequest(t, grantID, "DISBURSEMENT_FAILED", amount)
		(&WebhookHandler{}).HandleMonnifyWebhook(c)
		require.Equal(t, http.StatusOK, w.Code)
	}

	g, expense := grantState(t, orgID, grantID)
	assert.Equal(t, models.GrantFailed, g.Status)
	assert.Zero(t, expense.Minor)
}

func TestGrantWebhook_AmountMismatchIsRejected(t *testing.T) {
	skipIfNoDB(t)
	orgID, grantID := seedSubmittedGrant(t, money.FromNaira(20_000))

	w, c := signedEWAWebhookRequest(t, grantID, "DISBURSEMENT_SUCCESSFUL", money.FromNaira(2_000))
	(&WebhookHandler{}).HandleMonnifyWebhook(c)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)

	g, _ := grantState(t, orgID, grantID)
	assert.Equal(t, models.GrantSubmitted, g.Status)
}
