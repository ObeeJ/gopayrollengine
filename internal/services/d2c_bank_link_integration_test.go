//go:build integration

package services

import (
	"context"
	"testing"
	"time"

	"go-payroll-engine/internal/integrations/banklink"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// seedD2CWorker creates a D2C org (is_d2c = true) with a single employee —
// the worker themself, not staff of a business — the same synthetic tenant
// shape migration 000030's comment describes.
func seedD2CWorker(t *testing.T) (orgID, employeeID string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	employeeID = "EMP-" + uuid.New().String()[:8]

	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, currency, is_d2c, created_at, updated_at) VALUES (?, ?, ?, TRUE, NOW(), NOW())",
		orgID, "d2c test worker", string(money.NGN),
	).Error)

	require.NoError(t, models.DB.Create(&models.Employee{
		ID:             employeeID,
		OrganizationID: orgID,
		Name:           "Tunde",
		Email:          models.EncryptedString("tunde-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		IsActive:       true,
	}).Error)

	return orgID, employeeID
}

// A bank link persists tenant-scoped like every other table — readable
// inside WithOrgScope, invisible outside it, RLS doing the isolation.
func TestD2CBankLink_PersistsAndIsOrgScoped(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CWorker(t)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Create(&models.D2CBankLink{
			OrganizationID:     orgID,
			EmployeeID:         employeeID,
			Provider:           "mock",
			ProviderAccountRef: "mock-acct-123",
			LinkedAt:           time.Now(),
		}).Error
	}))

	var link models.D2CBankLink
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return tx.Where("employee_id = ?", employeeID).First(&link).Error
	}))
	assert.Equal(t, models.D2CBankLinkLinked, link.Status)
	assert.Equal(t, "mock-acct-123", link.ProviderAccountRef)

	// A different org must never see it — the same RLS isolation every
	// other tenant-scoped table already relies on. setupRLSTestRole pivots
	// off the superuser connection this test binary otherwise uses:
	// superusers bypass RLS entirely, so this assertion would pass for the
	// wrong reason (and prove nothing) without it — see that helper's comment.
	otherOrgID, _ := seedD2CWorker(t)
	setupRLSTestRole(t)
	var count int64
	require.NoError(t, models.WithOrgScope(context.Background(), otherOrgID, func(tx *gorm.DB) error {
		if err := tx.Exec("SET LOCAL ROLE rls_test_user").Error; err != nil {
			return err
		}
		return tx.Model(&models.D2CBankLink{}).Where("employee_id = ?", employeeID).Count(&count).Error
	}))
	assert.Zero(t, count, "a bank link must not be visible from a different org's scope")
}

// Only one active ("linked") link per employee per provider — relinking
// must not silently create a second row an app bug could pick either of.
func TestD2CBankLink_OnlyOneActiveLinkPerProvider(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedD2CWorker(t)

	create := func() error {
		return models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
			return tx.Create(&models.D2CBankLink{
				OrganizationID:     orgID,
				EmployeeID:         employeeID,
				Provider:           "mock",
				ProviderAccountRef: "mock-acct-" + uuid.New().String()[:8],
				LinkedAt:           time.Now(),
			}).Error
		})
	}
	require.NoError(t, create())
	require.Error(t, create(), "a second 'linked' row for the same employee+provider must be rejected")
}

// End-to-end: the mock provider's transaction history flows straight into
// PredictNextPayday — the seam this whole feature is built around.
func TestD2CBankLink_MockProviderTransactionsFeedPaydayPrediction(t *testing.T) {
	skipIfNoDB(t)
	mock := banklink.NewMock()
	mock.Transactions["mock-acct-1"] = recurringCredits(4, 30, 1, money.FromNaira(300_000))

	txs, err := mock.GetTransactions(context.Background(), "mock-acct-1", predictionNow.AddDate(0, -6, 0))
	require.NoError(t, err)

	pred, err := PredictNextPayday(txs, predictionNow)
	require.NoError(t, err)
	assert.Equal(t, ConfidenceHigh, pred.Confidence)
}
