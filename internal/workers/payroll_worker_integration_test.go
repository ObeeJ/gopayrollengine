//go:build integration

package workers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestMain(m *testing.M) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		os.Exit(m.Run())
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		panic(err)
	}
	models.DB = db
	models.InitEncryption()
	os.Exit(m.Run())
}

func skipIfNoDB(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set — skipping worker integration test")
	}
}

// seedPendingBatch creates an org, employees, and a pending payroll with N
// items. pending_count is intentionally left at 0 to model the pre-worker
// state — the worker must set it as part of the FSM transition.
func seedPendingBatch(t *testing.T, n int) (orgID, payrollID string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	payrollID = "PAY-" + uuid.New().String()[:8]

	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		orgID, "worker test org",
	).Error)

	require.NoError(t, models.DB.Create(&models.Payroll{
		ID:             payrollID,
		OrganizationID: orgID,
		Period:         "worker-" + uuid.New().String()[:8],
		TotalAmount:    money.FromNaira(int64(n) * 1000),
		Status:         models.PayrollPending,
		PendingCount:   0, // pre-worker state — must be N after the worker runs
	}).Error)

	for i := 0; i < n; i++ {
		empID := "EMP-" + uuid.New().String()[:8]
		require.NoError(t, models.DB.Create(&models.Employee{
			ID:             empID,
			OrganizationID: orgID,
			Name:           "Worker",
			Email:          models.EncryptedString("w-" + uuid.New().String()[:8] + "@example.com"),
			AccountNumber:  models.EncryptedString("0123456789"),
			BankCode:       models.EncryptedString("058"),
			Salary:         money.FromNaira(1000),
			IsActive:       true,
		}).Error)
		require.NoError(t, models.DB.Create(&models.PayrollItem{
			ID:             "ITEM-" + uuid.New().String()[:8],
			OrganizationID: orgID,
			PayrollID:      payrollID,
			EmployeeID:     empID,
			EmployeeName:   "Worker",
			Amount:         money.FromNaira(1000),
			Status:         models.PayrollPending,
		}).Error)
	}
	return orgID, payrollID
}

// fakeMonnify spins up an httptest server speaking the two endpoints the worker
// touches: /api/v1/auth/login and /api/v1/disbursements/batch. On the bulk
// transfer call it captures the *current* DB state of the payroll under test,
// so the test can assert ordering: pending_count must already be N by the time
// Monnify is invoked. This is the deterministic proof that the FSM transition
// and counter init happen BEFORE the network call.
type fakeMonnify struct {
	server          *httptest.Server
	observedPending int
	observedStatus  models.PayrollStatus
	mu              sync.Mutex
	payrollID       string
}

func newFakeMonnify(t *testing.T, payrollID string) *fakeMonnify {
	t.Helper()
	fm := &fakeMonnify{payrollID: payrollID}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"requestSuccessful": true,
			"responseBody": map[string]interface{}{
				"accessToken": "test-token",
				"expiresIn":   3600,
			},
		})
	})
	mux.HandleFunc("/api/v1/disbursements/batch", func(w http.ResponseWriter, r *http.Request) {
		// Capture DB state at the moment Monnify is called.
		type rowT struct {
			PendingCount int
			Status       models.PayrollStatus
		}
		var row rowT
		_ = models.DB.Raw(
			"SELECT pending_count, status FROM payrolls WHERE id = ?", fm.payrollID,
		).Scan(&row).Error
		fm.mu.Lock()
		fm.observedPending = row.PendingCount
		fm.observedStatus = row.Status
		fm.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"requestSuccessful": true,
			"responseMessage":   "ok",
			"responseBody": map[string]interface{}{
				"batchReference": fm.payrollID,
				"status":         "SUCCESSFUL",
			},
		})
	})
	fm.server = httptest.NewServer(mux)
	return fm
}

// TestProcessPayrollTask_CounterSetBeforeMonnify is the load-bearing assertion
// for Fix #3. The fake Monnify server records the payroll's pending_count at
// the instant the network call is received. With the fix in place this must
// equal N. Pre-fix it would be 0 — and a webhook that raced ahead would
// decrement 0 → -1 and have its decrement clobbered by the post-Monnify
// counter overwrite.
func TestProcessPayrollTask_CounterSetBeforeMonnify(t *testing.T) {
	skipIfNoDB(t)
	const n = 5
	orgID, payrollID := seedPendingBatch(t, n)
	fm := newFakeMonnify(t, payrollID)
	defer fm.server.Close()

	t.Setenv("MOCK_MODE", "false")
	t.Setenv("MONNIFY_BASE_URL", fm.server.URL)
	t.Setenv("MONNIFY_API_KEY", "test")
	t.Setenv("MONNIFY_SECRET_KEY", "test")
	t.Setenv("MONNIFY_SOURCE_WALLET", "9999999999")

	h := &PayrollHandler{MonnifyClient: monnify.NewClient()}
	payload, _ := json.Marshal(map[string]string{"payroll_id": payrollID, "org_id": orgID})
	task := asynq.NewTask(TypeProcessPayroll, payload)

	require.NoError(t, h.ProcessPayrollTask(context.Background(), task))

	fm.mu.Lock()
	defer fm.mu.Unlock()
	assert.Equal(t, n, fm.observedPending,
		"pending_count must be N at the moment Monnify is invoked — proves FSM+counter happens BEFORE the network call")
	assert.Equal(t, models.PayrollProcessing, fm.observedStatus,
		"status must already be 'processing' when Monnify is invoked")

	// Final state sanity.
	var final models.Payroll
	require.NoError(t, models.DB.First(&final, "id = ?", payrollID).Error)
	assert.Equal(t, models.PayrollProcessing, final.Status)
	assert.Equal(t, n, final.PendingCount)
}

// TestProcessPayrollTask_DuplicateTaskCASRejected proves the FSM+counter
// UPDATE is a CAS: a second worker picking up the same task (Asynq retry,
// duplicate enqueue) cannot transition an already-processing payroll. Without
// the WHERE status='pending' clause both workers would overwrite the counter.
func TestProcessPayrollTask_DuplicateTaskCASRejected(t *testing.T) {
	skipIfNoDB(t)
	orgID, payrollID := seedPendingBatch(t, 3)

	// Pre-flip to processing to simulate "another worker already grabbed it".
	require.NoError(t, models.DB.Exec(
		"UPDATE payrolls SET status = ?, pending_count = ? WHERE id = ?",
		models.PayrollProcessing, 3, payrollID,
	).Error)

	t.Setenv("MOCK_MODE", "true")
	h := &PayrollHandler{MonnifyClient: monnify.NewClient()}
	payload, _ := json.Marshal(map[string]string{"payroll_id": payrollID, "org_id": orgID})
	task := asynq.NewTask(TypeProcessPayroll, payload)

	err := h.ProcessPayrollTask(context.Background(), task)
	require.Error(t, err, "duplicate task must be rejected by the CAS")
	assert.Contains(t, err.Error(), "not retryable")

	// Counter must not have been clobbered.
	var pc int
	require.NoError(t, models.DB.Raw(
		"SELECT pending_count FROM payrolls WHERE id = ?", payrollID,
	).Scan(&pc).Error)
	assert.Equal(t, 3, pc, "CAS rejection must leave pending_count untouched")
}
