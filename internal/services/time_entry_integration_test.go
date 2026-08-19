//go:build integration

package services

import (
	"context"
	"testing"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedHourlyWorker creates an org with one active hourly employee at the
// given rate — the counterpart to seedWorker for salaried staff.
func seedHourlyWorker(t *testing.T, hourlyRateKobo money.Kobo) (orgID, employeeID string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	employeeID = "EMP-" + uuid.New().String()[:8]

	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		orgID, "hourly test org",
	).Error)

	require.NoError(t, models.DB.Create(&models.Employee{
		ID:             employeeID,
		OrganizationID: orgID,
		Name:           "Chidi",
		Email:          models.EncryptedString("chidi-" + uuid.New().String()[:8] + "@example.com"),
		AccountNumber:  models.EncryptedString("0123456789"),
		BankCode:       models.EncryptedString("058"),
		WageType:       models.WageHourly,
		HourlyRateKobo: hourlyRateKobo,
		IsActive:       true,
	}).Error)

	return orgID, employeeID
}

func TestSubmitTimeEntry_Succeeds(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_500))
	svc := NewTimeEntryService()

	entry, err := svc.SubmitTimeEntry(context.Background(), orgID, employeeID,
		time.Now().AddDate(0, 0, -1), 480, "covered the morning shift")
	require.NoError(t, err)
	assert.Equal(t, models.TimeEntryPending, entry.Status)
	assert.Equal(t, 480, entry.MinutesWorked)
}

func TestSubmitTimeEntry_RejectsSalariedEmployee(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewTimeEntryService()

	_, err := svc.SubmitTimeEntry(context.Background(), orgID, employeeID, time.Now(), 480, "")
	require.ErrorIs(t, err, ErrTimeEntryNotHourly)
}

func TestSubmitTimeEntry_RejectsFutureDate(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_500))
	svc := NewTimeEntryService()

	_, err := svc.SubmitTimeEntry(context.Background(), orgID, employeeID,
		time.Now().AddDate(0, 0, 1), 480, "")
	require.ErrorIs(t, err, ErrTimeEntryFutureDate)
}

func TestSubmitTimeEntry_RejectsOutOfRangeMinutes(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_500))
	svc := NewTimeEntryService()

	for _, minutes := range []int{0, -30, 1441} {
		_, err := svc.SubmitTimeEntry(context.Background(), orgID, employeeID, time.Now(), minutes, "")
		require.ErrorIsf(t, err, ErrTimeEntryInvalidRange, "minutes=%d should be rejected", minutes)
	}
}

func TestApproveTimeEntry_MovesToApprovedAndRecordsApprover(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_500))
	svc := NewTimeEntryService()

	entry, err := svc.SubmitTimeEntry(context.Background(), orgID, employeeID, time.Now(), 240, "")
	require.NoError(t, err)

	approved, err := svc.ApproveTimeEntry(context.Background(), orgID, entry.ID, "admin", "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, models.TimeEntryApproved, approved.Status)
	assert.Equal(t, "admin", approved.ApprovedBy)
	require.NotNil(t, approved.ApprovedAt)
}

// A second approval attempt (double-click, or a race between two admins) must
// be refused rather than silently succeeding a second time.
func TestApproveTimeEntry_DoubleApprovalIsRefused(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_500))
	svc := NewTimeEntryService()

	entry, err := svc.SubmitTimeEntry(context.Background(), orgID, employeeID, time.Now(), 240, "")
	require.NoError(t, err)

	_, err = svc.ApproveTimeEntry(context.Background(), orgID, entry.ID, "admin", "127.0.0.1")
	require.NoError(t, err)

	_, err = svc.ApproveTimeEntry(context.Background(), orgID, entry.ID, "admin", "127.0.0.1")
	require.ErrorIs(t, err, ErrTimeEntryAlreadyResolved)
}

func TestRejectTimeEntry_RecordsReason(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_500))
	svc := NewTimeEntryService()

	entry, err := svc.SubmitTimeEntry(context.Background(), orgID, employeeID, time.Now(), 240, "")
	require.NoError(t, err)

	rejected, err := svc.RejectTimeEntry(context.Background(), orgID, entry.ID, "admin", "127.0.0.1", "does not match the shift log")
	require.NoError(t, err)
	assert.Equal(t, models.TimeEntryRejected, rejected.Status)
	assert.Equal(t, "does not match the shift log", rejected.RejectionReason)
}

func TestListTimeEntries_FiltersByStatus(t *testing.T) {
	skipIfNoDB(t)
	orgID, employeeID := seedHourlyWorker(t, money.FromNaira(1_500))
	svc := NewTimeEntryService()

	pending, err := svc.SubmitTimeEntry(context.Background(), orgID, employeeID, time.Now(), 120, "")
	require.NoError(t, err)
	toApprove, err := svc.SubmitTimeEntry(context.Background(), orgID, employeeID, time.Now().AddDate(0, 0, -1), 180, "")
	require.NoError(t, err)
	_, err = svc.ApproveTimeEntry(context.Background(), orgID, toApprove.ID, "admin", "127.0.0.1")
	require.NoError(t, err)

	pendingOnly, err := svc.ListTimeEntries(context.Background(), orgID, employeeID, models.TimeEntryPending, 0)
	require.NoError(t, err)
	require.Len(t, pendingOnly, 1)
	assert.Equal(t, pending.ID, pendingOnly[0].ID)

	all, err := svc.ListTimeEntries(context.Background(), orgID, employeeID, "", 0)
	require.NoError(t, err)
	assert.Len(t, all, 2)
}
