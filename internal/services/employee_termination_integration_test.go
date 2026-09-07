//go:build integration

package services

import (
	"context"
	"testing"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestTerminate_DeactivatesEmployee(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEmployeeTerminationService()

	emp, err := svc.Terminate(context.Background(), orgID, employeeID, "resigned", "127.0.0.1")
	require.NoError(t, err)
	assert.False(t, emp.IsActive)

	var reloaded models.Employee
	require.NoError(t, models.DB.First(&reloaded, "id = ?", employeeID).Error)
	assert.False(t, reloaded.IsActive)
}

// An Approved-but-undisbursed advance at termination: cash never left, so it
// is cancelled — the receivable reverses to zero, same as any other
// cancellation.
func TestTerminate_CancelsApprovedAdvance(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	ewaSvc := NewEWAService()
	advance, _, err := ewaSvc.RequestAdvance(context.Background(), orgID, employeeID, money.FromNaira(1_000), "term-approved", "127.0.0.1")
	require.NoError(t, err)
	require.Equal(t, models.AdvanceApproved, advance.Status)

	termSvc := NewEmployeeTerminationService()
	_, err = termSvc.Terminate(context.Background(), orgID, employeeID, "resigned", "127.0.0.1")
	require.NoError(t, err)

	var reloaded models.EWAAdvance
	require.NoError(t, models.DB.First(&reloaded, "id = ?", advance.ID).Error)
	assert.Equal(t, models.AdvanceCancelled, reloaded.Status)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		recv, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
		require.NoError(t, err)
		bal, err := models.AccountBalance(tx, recv.ID)
		require.NoError(t, err)
		assert.Equal(t, money.Money{Currency: money.NGN}, bal, "a cancelled advance must leave no receivable")
		return nil
	}))
}

// A Disbursed advance at termination: cash did leave, so it is written off
// — the receivable zeroes out against write_off_expense, not cash_settlement.
func TestTerminate_WritesOffDisbursedAdvance(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	ewaSvc := NewEWAService()
	advance, _, err := ewaSvc.RequestAdvance(context.Background(), orgID, employeeID, money.FromNaira(1_000), "term-disbursed", "127.0.0.1")
	require.NoError(t, err)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.ConfirmDisbursed(tx, advance)
	}))

	termSvc := NewEmployeeTerminationService()
	_, err = termSvc.Terminate(context.Background(), orgID, employeeID, "resigned", "127.0.0.1")
	require.NoError(t, err)

	var reloaded models.EWAAdvance
	require.NoError(t, models.DB.First(&reloaded, "id = ?", advance.ID).Error)
	assert.Equal(t, models.AdvanceWrittenOff, reloaded.Status)

	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		recv, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
		require.NoError(t, err)
		bal, err := models.AccountBalance(tx, recv.ID)
		require.NoError(t, err)
		assert.Equal(t, money.Money{Currency: money.NGN}, bal, "a written-off advance must leave no receivable")

		writeOff, err := models.EnsureAccount(tx, orgID, "", models.AccountWriteOffExpense, money.NGN)
		require.NoError(t, err)
		woBal, err := models.AccountBalance(tx, writeOff.ID)
		require.NoError(t, err)
		assert.Equal(t, money.Money{Minor: 1_000 * 100, Currency: money.NGN}, woBal,
			"the loss must be booked on write_off_expense")
		return nil
	}))
}

// Idempotent: calling Terminate a second time must not error or double-audit.
func TestTerminate_SecondCallIsIdempotent(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEmployeeTerminationService()

	_, err := svc.Terminate(context.Background(), orgID, employeeID, "resigned", "127.0.0.1")
	require.NoError(t, err)
	_, err = svc.Terminate(context.Background(), orgID, employeeID, "resigned", "127.0.0.1")
	require.NoError(t, err)

	var count int64
	require.NoError(t, models.DB.Model(&models.AuditEvent{}).
		Where("organization_id = ? AND entity_type = ? AND entity_id = ? AND action = ?",
			orgID, "Employee", employeeID, "terminated").
		Count(&count).Error)
	assert.Equal(t, int64(1), count, "a second termination call must not audit again")
}

// An already-settled advance must not be touched by termination — it is no
// longer outstanding, and re-resolving it would be a ledger error.
func TestTerminate_LeavesSettledAdvancesAlone(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	ewaSvc := NewEWAService()
	advance, _, err := ewaSvc.RequestAdvance(context.Background(), orgID, employeeID, money.FromNaira(1_000), "term-settled", "127.0.0.1")
	require.NoError(t, err)
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.ConfirmDisbursed(tx, advance)
	}))
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.TransitionAdvance(tx, advance, models.AdvanceDisbursed, models.AdvanceSettled)
	}))

	termSvc := NewEmployeeTerminationService()
	_, err = termSvc.Terminate(context.Background(), orgID, employeeID, "resigned", "127.0.0.1")
	require.NoError(t, err)

	var reloaded models.EWAAdvance
	require.NoError(t, models.DB.First(&reloaded, "id = ?", advance.ID).Error)
	assert.Equal(t, models.AdvanceSettled, reloaded.Status, "a settled advance must not be reclassified")
}
