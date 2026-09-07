//go:build integration

package services

import (
	"context"
	"testing"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccrualSnapshotCollector_SnapshotsSalariedEmployee(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	now := time.Now()

	collector := NewAccrualSnapshotCollector()
	require.NoError(t, collector.Collect(context.Background(), now))

	var snap models.EWAAccrualSnapshot
	require.NoError(t, models.DB.Where("organization_id = ? AND employee_id = ?", orgID, employeeID).
		First(&snap).Error)
	assert.Equal(t, models.WageSalaried, snap.WageType)
	assert.Equal(t, money.FromNaira(300_000), snap.SalaryKobo)
	assert.Equal(t, now.Format(PeriodLayout), snap.Period)

	expected, err := AccruedToDate(money.FromNaira(300_000), now.Format(PeriodLayout), now)
	require.NoError(t, err)
	assert.Equal(t, expected, snap.AccruedKobo)
}

func TestAccrualSnapshotCollector_SnapshotsHourlyEmployee(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_500))
	teSvc := NewTimeEntryService()
	now := time.Now()

	approved, err := teSvc.SubmitTimeEntry(context.Background(), orgID, employeeID, now, 240, "")
	require.NoError(t, err)
	_, err = teSvc.ApproveTimeEntry(context.Background(), orgID, approved.ID, "admin", "127.0.0.1")
	require.NoError(t, err)

	collector := NewAccrualSnapshotCollector()
	require.NoError(t, collector.Collect(context.Background(), now))

	var snap models.EWAAccrualSnapshot
	require.NoError(t, models.DB.Where("organization_id = ? AND employee_id = ?", orgID, employeeID).
		First(&snap).Error)
	assert.Equal(t, models.WageHourly, snap.WageType)
	assert.Equal(t, money.FromNaira(1_500), snap.HourlyRateKobo)
	// 4h approved at ₦1,500/hr.
	assert.Equal(t, money.FromNaira(6_000), snap.AccruedKobo)
}

// Re-running the collector for the same day must correct that day's row, not
// pile up a duplicate — the whole point of the upsert on (org, employee, date).
func TestAccrualSnapshotCollector_SameDayRerunUpserts(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	now := time.Now()

	collector := NewAccrualSnapshotCollector()
	require.NoError(t, collector.Collect(context.Background(), now))
	require.NoError(t, collector.Collect(context.Background(), now))

	var count int64
	require.NoError(t, models.DB.Model(&models.EWAAccrualSnapshot{}).
		Where("organization_id = ? AND employee_id = ?", orgID, employeeID).
		Count(&count).Error)
	assert.Equal(t, int64(1), count, "a second run on the same day must upsert, not duplicate")
}

// An inactive employee must never accrue a snapshot — there is nothing to
// reconstruct a dispute over for someone no longer drawing EWA.
func TestAccrualSnapshotCollector_SkipsInactiveEmployee(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	require.NoError(t, models.DB.Model(&models.Employee{}).Where("id = ?", employeeID).
		Update("is_active", false).Error)

	collector := NewAccrualSnapshotCollector()
	require.NoError(t, collector.Collect(context.Background(), time.Now()))

	var count int64
	require.NoError(t, models.DB.Model(&models.EWAAccrualSnapshot{}).
		Where("organization_id = ? AND employee_id = ?", orgID, employeeID).
		Count(&count).Error)
	assert.Zero(t, count)
}
