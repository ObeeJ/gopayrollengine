//go:build integration

package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"go-payroll-engine/internal/integrations/provider"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedWorkerWithCurrency mirrors seedWorker, but for an org operating in a
// currency other than the implicit NGN every other seed helper assumes.
func seedWorkerWithCurrency(t *testing.T, currency money.Currency, salary money.Kobo) (orgID, employeeID string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	employeeID = "EMP-" + uuid.New().String()[:8]

	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, currency, created_at, updated_at) VALUES (?, ?, ?, NOW(), NOW())",
		orgID, "multi-currency test org", string(currency),
	).Error)

	require.NoError(t, models.DB.Create(&models.Employee{
		ID:             employeeID,
		OrganizationID: orgID,
		Name:           "Kwame",
		Email:          models.EncryptedString("kwame-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		Salary:         salary,
		IsActive:       true,
	}).Error)

	return orgID, employeeID
}

// An EWA advance for a non-NGN org must post its ledger entries in that
// org's own currency, not NGN — the entire reason OrgCurrencyTx exists
// instead of every ledger call site hardcoding money.NGN.
func TestRequestAdvance_NonNGNOrg_PostsLedgerInOrgCurrency(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorkerWithCurrency(t, money.GHS, money.FromNaira(300_000))
	svc := NewEWAService()

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, time.Now())
	require.NoError(t, err)
	require.False(t, el.Blocked, "a fresh worker mid-period should be eligible: %s", el.BlockedReason)
	require.True(t, el.Available.IsPositive())

	advance, _, err := svc.RequestAdvance(
		context.Background(), orgID, employeeID, el.MinimumDraw, "idem-ghs-1", "127.0.0.1")
	require.NoError(t, err)
	require.Equal(t, models.AdvanceApproved, advance.Status)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		recv, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.GHS)
		require.NoError(t, err)
		assert.Equal(t, money.GHS, recv.Currency)

		bal, err := models.AccountBalance(tx, recv.ID)
		require.NoError(t, err)
		assert.Equal(t, money.GHS, bal.Currency)
		assert.Equal(t, money.KoboIn(money.GHS, el.MinimumDraw), bal,
			"the advance must appear as a GHS receivable, not an NGN one")
		return nil
	}))
}

// DefaultEWAPolicy for a non-default-currency org must produce a policy in
// that currency, not Naira reinterpreted as another currency's minor units.
func TestGetEligibility_NonNGNOrg_UsesCurrencyAppropriateDefaultPolicy(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorkerWithCurrency(t, money.GHS, money.FromNaira(300_000))
	svc := NewEWAService()

	el, err := svc.GetEligibility(context.Background(), orgID, employeeID, time.Now())
	require.NoError(t, err)

	ghsDefault := models.DefaultEWAPolicy(orgID, money.GHS)
	assert.Equal(t, ghsDefault.MinDrawKobo, el.MinimumDraw)
	assert.NotEqual(t, models.DefaultEWAPolicy(orgID, money.NGN).MinDrawKobo, el.MinimumDraw,
		"must not fall back to the NGN default for a GHS org")
}

// CreatePayroll must refuse to run for an org whose currency no registered
// provider can settle — synchronously and clearly, rather than queuing a run
// that can only ever fail deep inside a currency-specific worker call. Every
// registered provider today (Monnify, Paystack) is NGN-only.
func TestCreatePayroll_RefusesOrgWithUnsupportedCurrency(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedWorkerWithCurrency(t, money.GHS, money.FromNaira(300_000))
	period := time.Now().Format(PeriodLayout)

	payrollSvc := NewPayrollService(
		repository.NewPayrollRepository(models.DB),
		repository.NewEmployeeRepository(models.DB),
	)
	_, err := payrollSvc.CreatePayroll(context.Background(), orgID, period)
	require.Error(t, err)
	assert.True(t, errors.Is(err, provider.ErrNoProviderForCurrency),
		"expected a no-provider-for-currency error, got: %v", err)

	// Nothing must have been written — the guard runs before any payroll or
	// item is created, inside the same transaction.
	var count int64
	require.NoError(t, models.DB.Model(&models.Payroll{}).
		Where("organization_id = ?", orgID).Count(&count).Error)
	assert.Zero(t, count, "no payroll should exist for an org whose currency cannot be disbursed")
}

// The exact same run must still succeed for an NGN org — the guard adds a
// check, not a new failure mode for the currency everything already works in.
func TestCreatePayroll_NGNOrg_UnaffectedByProviderGuard(t *testing.T) {
	skipIfNoDB(t)
	orgID, _ := seedWorker(t, money.FromNaira(300_000))
	period := time.Now().Format(PeriodLayout)

	payrollSvc := NewPayrollService(
		repository.NewPayrollRepository(models.DB),
		repository.NewEmployeeRepository(models.DB),
	)
	payroll, err := payrollSvc.CreatePayroll(context.Background(), orgID, period)
	require.NoError(t, err)
	assert.Equal(t, money.FromNaira(300_000), payroll.TotalAmount)
}
