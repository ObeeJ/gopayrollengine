//go:build integration

package services

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/testutil"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// These run under a production-shaped (NOSUPERUSER, NOBYPASSRLS) role. The
// rest of the suite connects as a superuser, which bypasses row-level
// security, so a tenant-table read outside WithOrgScope passes there while
// returning nothing in production. See testutil.UseAppRoleDB.

func seedCompletedPayroll(t *testing.T, orgID string, total money.Kobo) string {
	t.Helper()
	id := "PAY-" + uuid.New().String()[:8]
	require.NoError(t, models.DB.Create(&models.Payroll{
		ID:             id,
		OrganizationID: orgID,
		Period:         "2026-08",
		TotalAmount:    total,
		Status:         models.PayrollCompleted,
	}).Error)
	return id
}

func TestEvidenceCollector_SeesRealActivityUnderProductionRole(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(100_000))
	payrollID := seedCompletedPayroll(t, orgID, money.FromNaira(100_000))
	require.NoError(t, models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		return models.AppendAuditTx(tx, orgID, "Employee", employeeID, "evidence_test", "", "", "127.0.0.1", "")
	}))

	testutil.UseAppRoleDB(t)

	dir := t.TempDir()
	collector := &EvidenceCollector{outputDir: dir}
	now := time.Now().UTC()
	require.NoError(t, collector.Collect(context.Background(), now))

	raw, err := os.ReadFile(filepath.Join(dir, "soc2-"+now.Format("2006-01-02")+".json"))
	require.NoError(t, err)
	var snap EvidenceSnapshot
	require.NoError(t, json.Unmarshal(raw, &snap))

	assert.Positive(t, snap.AuditEventCount, "audit events must be visible to the collector in production")
	found := false
	for _, p := range snap.PayrollBatches {
		if p.ID == payrollID {
			found = true
		}
	}
	assert.True(t, found, "today's payroll batch must appear in the evidence")
}

func TestPredictiveCashFlow_ReadsHistoryUnderProductionRole(t *testing.T) {
	skipIfNoDB(t)
	t.Setenv("MOCK_MODE", "true")
	orgID, _ := seedWorker(t, money.FromNaira(100_000))
	seedCompletedPayroll(t, orgID, money.FromNaira(90_000))

	testutil.UseAppRoleDB(t)

	svc := NewAnalyticsService(
		repository.NewPayrollRepository(models.DB),
		repository.NewEmployeeRepository(models.DB),
	)
	res, err := svc.GetPredictiveCashFlow(context.Background(), orgID)
	require.NoError(t, err)
	assert.Equal(t, money.FromNaira(90_000), res.PredictedAmount,
		"the org's own completed payroll history must be visible under RLS")
}
