//go:build integration

package services

import (
	"context"
	"testing"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/testutil"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedProcessingItem(t *testing.T, orgID string, sentAgo time.Duration) {
	t.Helper()
	empID := "EMP-" + uuid.New().String()[:8]
	require.NoError(t, models.DB.Create(&models.Employee{
		ID: empID, OrganizationID: orgID, Name: "W",
		Email:         models.EncryptedString("w-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber: "0123456789", BankCode: "058", Salary: money.FromNaira(1000), IsActive: true,
	}).Error)
	payroll := models.Payroll{OrganizationID: orgID, Period: "scan-" + uuid.New().String()[:6],
		Status: models.PayrollProcessing, PendingCount: 1}
	require.NoError(t, models.DB.Create(&payroll).Error)
	sent := time.Now().Add(-sentAgo)
	require.NoError(t, models.DB.Create(&models.PayrollItem{
		OrganizationID: orgID, PayrollID: payroll.ID, EmployeeID: empID, EmployeeName: "W",
		Amount: money.FromNaira(1000), Status: models.PayrollProcessing, SentAt: &sent,
	}).Error)
}

func TestStuckPayoutScanner_CountsOnlyOverdueItemsPerOrganisation(t *testing.T) {
	skipIfNoDB(t)
	orgA, _ := seedWorker(t, money.FromNaira(100_000))
	orgB, _ := seedWorker(t, money.FromNaira(100_000))
	seedProcessingItem(t, orgA, models.StuckAfter+time.Hour)
	seedProcessingItem(t, orgA, models.StuckAfter+2*time.Hour)
	seedProcessingItem(t, orgA, time.Minute) // fresh: its callback is still due
	seedProcessingItem(t, orgB, models.StuckAfter+time.Minute)

	// Production role: the scan must read each org under its own RLS scope,
	// not rely on a connection that sees everything.
	testutil.UseAppRoleDB(t)
	got, err := NewStuckPayoutScanner(time.Minute).Scan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, got[orgA])
	assert.Equal(t, 1, got[orgB])
}
