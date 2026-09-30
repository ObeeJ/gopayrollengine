//go:build integration

package workers

import (
	"context"
	"encoding/json"
	"testing"

	"go-payroll-engine/internal/integrations/provider"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/testutil"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedPendingGrant — a pending grant plus the Dr expense / Cr cash entry
// IssueHardshipGrant posts with it (the services package can't be imported
// from here: it imports workers).
func seedPendingGrant(t *testing.T, amount money.Kobo) (orgID, grantID string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	employeeID := "EMP-" + uuid.New().String()[:8]
	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		orgID, "grant worker test org").Error)
	require.NoError(t, models.DB.Create(&models.Employee{
		ID: employeeID, OrganizationID: orgID, Name: "Bola",
		Email:         models.EncryptedString("grant-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber: "0123456789", BankCode: "058", Salary: money.FromNaira(200_000), IsActive: true,
	}).Error)

	g := models.EWAHardshipGrant{OrganizationID: orgID, EmployeeID: employeeID, AmountKobo: amount,
		Reason: "medical", ApprovedByIP: "127.0.0.1"}
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
		_, err = models.PostTransaction(tx, models.PostingRequest{
			OrgID: orgID, Kind: "hardship_grant", Reference: g.ID, IdempotencyKey: "hardship_grant:" + g.ID,
			Entries: []models.EntryInput{
				{AccountID: expense.ID, Direction: models.Debit, Amount: money.NGNFromKobo(amount)},
				{AccountID: cash.ID, Direction: models.Credit, Amount: money.NGNFromKobo(amount)},
			},
		})
		return err
	}))
	return orgID, g.ID
}

func runGrantTask(t *testing.T, h *HardshipGrantDisbursementHandler, orgID, grantID string) error {
	t.Helper()
	payload, err := json.Marshal(map[string]string{"grant_id": grantID, "org_id": orgID})
	require.NoError(t, err)
	return h.ProcessHardshipGrantDisbursementTask(context.Background(), asynq.NewTask(TypeDisburseGrant, payload))
}

func loadGrant(t *testing.T, orgID, grantID string) (g models.EWAHardshipGrant, expense money.Money) {
	t.Helper()
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		if err := tx.First(&g, "id = ?", grantID).Error; err != nil {
			return err
		}
		acct, err := models.EnsureAccount(tx, orgID, "", models.AccountHardshipGrantExpense, money.NGN)
		if err != nil {
			return err
		}
		expense, err = models.AccountBalance(tx, acct.ID)
		return err
	}))
	return g, expense
}

// The bug this closes: a grant used to be booked as paid with nothing ever
// asking a bank to pay it. The worker must actually submit the transfer.
func TestGrantDisbursement_AcceptedSubmitsTransfer(t *testing.T) {
	skipIfNoDB(t)
	amount := money.FromNaira(15_000)
	orgID, grantID := seedPendingGrant(t, amount)
	testutil.UseAppRoleDB(t) // production-shaped role: RLS applies
	fp := &fakeProvider{name: "fake", result: provider.TransferResult{Accepted: true, ProviderReference: "REF-" + grantID}}

	require.NoError(t, runGrantTask(t, NewHardshipGrantDisbursementHandler(provider.NewRegistry(fp)), orgID, grantID))

	require.Len(t, fp.requests, 1)
	assert.Equal(t, grantID, fp.requests[0].Reference, "our grant ID is the provider idempotency key")
	assert.Equal(t, amount, money.Kobo(fp.requests[0].Amount.Minor))
	assert.Equal(t, "0123456789", fp.requests[0].RecipientAccountNumber)

	g, _ := loadGrant(t, orgID, grantID)
	assert.Equal(t, models.GrantSubmitted, g.Status, "accepted is not disbursed — only the webhook confirms")
	require.NotNil(t, g.ProviderReference)
	assert.Equal(t, "REF-"+grantID, *g.ProviderReference)
	assert.Nil(t, g.DisbursedAt)

	// A duplicate delivery of the task must not submit again.
	require.NoError(t, runGrantTask(t, NewHardshipGrantDisbursementHandler(provider.NewRegistry(fp)), orgID, grantID))
	assert.Len(t, fp.requests, 1)
}

func TestGrantDisbursement_RejectedFailsAndReversesLedger(t *testing.T) {
	skipIfNoDB(t)
	orgID, grantID := seedPendingGrant(t, money.FromNaira(15_000))
	testutil.UseAppRoleDB(t)
	fp := &fakeProvider{name: "fake", result: provider.TransferResult{Accepted: false, Message: "invalid account"}}

	require.NoError(t, runGrantTask(t, NewHardshipGrantDisbursementHandler(provider.NewRegistry(fp)), orgID, grantID))

	g, expense := loadGrant(t, orgID, grantID)
	assert.Equal(t, models.GrantFailed, g.Status)
	require.NotNil(t, g.FailureReason)
	assert.Contains(t, *g.FailureReason, "invalid account")
	assert.Zero(t, expense.Minor, "a failed grant's expense must be fully reversed")
}

func TestGrantDisbursement_ProviderUnavailableIsRetriedAndStaysPending(t *testing.T) {
	skipIfNoDB(t)
	orgID, grantID := seedPendingGrant(t, money.FromNaira(15_000))
	fp := &fakeProvider{name: "fake", err: provider.ErrProviderUnavailable}

	err := runGrantTask(t, NewHardshipGrantDisbursementHandler(provider.NewRegistry(fp)), orgID, grantID)
	require.Error(t, err)
	assert.NotErrorIs(t, err, asynq.SkipRetry, "an unreachable rail is transient")

	g, expense := loadGrant(t, orgID, grantID)
	assert.Equal(t, models.GrantPending, g.Status)
	assert.Equal(t, money.FromNaira(15_000), money.Kobo(expense.Minor), "nothing reversed while the outcome is unknown")
}

func TestGrantDisbursement_NoProviderFailsGrantWithoutRetry(t *testing.T) {
	skipIfNoDB(t)
	orgID, grantID := seedPendingGrant(t, money.FromNaira(15_000))

	err := runGrantTask(t, NewHardshipGrantDisbursementHandler(provider.NewRegistry()), orgID, grantID)
	require.ErrorIs(t, err, asynq.SkipRetry)

	g, expense := loadGrant(t, orgID, grantID)
	assert.Equal(t, models.GrantFailed, g.Status)
	assert.Zero(t, expense.Minor)
}
