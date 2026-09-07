//go:build integration

package services

import (
	"context"
	"testing"

	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The first-ever run has no prior row to diff against — it must record a
// baseline, not attempt a comparison against nothing.
func TestReconciliationJob_FirstRunHasNoDrift(t *testing.T) {
	skipIfNoDB(t)
	require.NoError(t, models.DB.Exec("TRUNCATE reconciliation_runs").Error)

	job := NewReconciliationJob(monnify.NewClient(), money.FromNaira(1_000))
	run, err := job.Run(context.Background())
	require.NoError(t, err)
	assert.Nil(t, run.DriftKobo)
	assert.False(t, run.Alerted)
}

// MOCK_MODE's wallet balance is fixed (₦1,000,000 every call — see
// monnify.Client.GetWalletBalance), so a second run with no ledger activity
// in between must see a zero wallet delta. If cash_settlement also hasn't
// moved, drift must be exactly zero.
func TestReconciliationJob_NoActivityMeansNoDrift(t *testing.T) {
	skipIfNoDB(t)
	require.NoError(t, models.DB.Exec("TRUNCATE reconciliation_runs").Error)

	job := NewReconciliationJob(monnify.NewClient(), money.FromNaira(1_000))
	_, err := job.Run(context.Background())
	require.NoError(t, err)

	run, err := job.Run(context.Background())
	require.NoError(t, err)
	require.NotNil(t, run.DriftKobo)
	assert.Equal(t, money.Zero, *run.DriftKobo)
	assert.False(t, run.Alerted)
}

// An advance disbursed between two runs moves cash_settlement but — under
// MOCK_MODE's fixed wallet balance — leaves the "wallet" side of the delta
// at zero, so the drift must equal exactly the ledger-side movement. This is
// the job doing its job: a real, unexplained movement shows up as drift.
func TestReconciliationJob_LedgerMovementWithoutWalletMovementIsDrift(t *testing.T) {
	skipIfNoDB(t)
	require.NoError(t, models.DB.Exec("TRUNCATE reconciliation_runs").Error)

	job := NewReconciliationJob(monnify.NewClient(), money.FromNaira(500))
	_, err := job.Run(context.Background())
	require.NoError(t, err)

	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()
	advance, _, err := svc.RequestAdvance(context.Background(), orgID, employeeID, money.FromNaira(1_000), "recon-1", "127.0.0.1")
	require.NoError(t, err)
	require.Equal(t, models.AdvanceApproved, advance.Status)

	run, err := job.Run(context.Background())
	require.NoError(t, err)
	require.NotNil(t, run.DriftKobo)
	// cash_settlement grew by ₦1,000 (Credit-normal, advance disbursed);
	// wallet balance under MOCK_MODE didn't move at all, so
	// drift = ΔWallet(0) + ΔCashSettlement(+1,000) = +1,000.
	assert.Equal(t, money.FromNaira(1_000), *run.DriftKobo)
	assert.True(t, run.Alerted, "drift equal to the threshold's neighbourhood must alert")
}

func TestReconciliationJob_DriftWithinThresholdDoesNotAlert(t *testing.T) {
	skipIfNoDB(t)
	require.NoError(t, models.DB.Exec("TRUNCATE reconciliation_runs").Error)

	job := NewReconciliationJob(monnify.NewClient(), money.FromNaira(5_000))
	_, err := job.Run(context.Background())
	require.NoError(t, err)

	orgID, employeeID := seedWorker(t, money.FromNaira(300_000))
	svc := NewEWAService()
	_, _, err = svc.RequestAdvance(context.Background(), orgID, employeeID, money.FromNaira(1_000), "recon-2", "127.0.0.1")
	require.NoError(t, err)

	run, err := job.Run(context.Background())
	require.NoError(t, err)
	require.NotNil(t, run.DriftKobo)
	assert.False(t, run.Alerted, "₦1,000 drift under a ₦5,000 threshold must not alert")
}
