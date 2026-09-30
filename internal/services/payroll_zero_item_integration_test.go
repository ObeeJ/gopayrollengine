//go:build integration

package services

import (
	"context"
	"testing"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An hourly worker with no approved hours nets to zero. A zero-amount line
// must never reach the bank — rails reject it, which can fail the whole batch
// for every other employee in it — so it's settled at creation instead and
// the worker (which only submits pending items) never sends it.
func TestCreatePayroll_ZeroNetItemIsSettledNotSent(t *testing.T) {
	skipIfNoDB(t)
	orgID, hourlyID := seedHourlyWorker(t, money.FromNaira(2_000))
	salariedID := "EMP-" + uuid.New().String()[:8]
	require.NoError(t, models.DB.Create(&models.Employee{
		ID:             salariedID,
		OrganizationID: orgID,
		Name:           "Salaried",
		Email:          models.EncryptedString("s-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		Salary:         money.FromNaira(150_000),
		IsActive:       true,
	}).Error)

	svc := NewPayrollService(repository.NewPayrollRepository(models.DB), repository.NewEmployeeRepository(models.DB))
	payroll, err := svc.CreatePayroll(context.Background(), orgID, "2026-08")
	require.NoError(t, err)

	var items []models.PayrollItem
	require.NoError(t, models.DB.Where("payroll_id = ?", payroll.ID).Find(&items).Error)
	require.Len(t, items, 2)
	for _, it := range items {
		switch it.EmployeeID {
		case hourlyID:
			assert.True(t, it.Amount.IsZero())
			assert.Equal(t, models.PayrollCompleted, it.Status, "zero-net item must be settled, not left to be sent")
		case salariedID:
			assert.Equal(t, money.FromNaira(150_000), it.Amount)
			assert.Equal(t, models.PayrollPending, it.Status)
		}
	}

	_, err = svc.CreatePayroll(context.Background(), orgID, "2026-8")
	assert.ErrorIs(t, err, ErrInvalidPeriod, "a second spelling of the same month must be refused, not paid again")
}
