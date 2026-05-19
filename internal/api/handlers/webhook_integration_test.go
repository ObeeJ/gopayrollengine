//go:build integration

package handlers

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// TestMain bootstraps encryption and wires the global models.DB to the test
// database. The webhook handler still reads models.DB directly (architectural
// debt), so this test must mirror that wiring.
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
	gin.SetMode(gin.TestMode)
	os.Setenv("MONNIFY_SECRET_KEY", "test-webhook-secret")
	os.Exit(m.Run())
}

func skipIfNoDB(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set — skipping webhook integration test")
	}
}

// seedConcurrentBatch creates an org, payroll, and N items in 'processing'
// state with pending_count = N. Mirrors the post-Monnify state immediately
// before webhooks begin arriving.
func seedConcurrentBatch(t *testing.T, n int) (orgID, payrollID string, itemIDs []string) {
	t.Helper()
	orgID = "ORG-" + uuid.New().String()[:8]
	payrollID = "PAY-" + uuid.New().String()[:8]

	require.NoError(t, models.DB.Exec(
		"INSERT INTO organizations (id, name, created_at, updated_at) VALUES (?, ?, NOW(), NOW())",
		orgID, "concurrency test org",
	).Error)

	require.NoError(t, models.DB.Create(&models.Payroll{
		ID:             payrollID,
		OrganizationID: orgID,
		Period:         "concurrency-" + uuid.New().String()[:8],
		TotalAmount:    money.FromNaira(int64(n) * 1000),
		Status:         models.PayrollProcessing,
		PendingCount:   n,
	}).Error)

	// Each item needs a unique employee — schema enforces an FK + employee email unique.
	itemIDs = make([]string, n)
	for i := 0; i < n; i++ {
		empID := "EMP-" + uuid.New().String()[:8]
		require.NoError(t, models.DB.Create(&models.Employee{
			ID:             empID,
			OrganizationID: orgID,
			Name:           "Worker",
			Email:          models.EncryptedString("worker-" + uuid.New().String()[:8] + "@example.com"),
			AccountNumber:  models.EncryptedString("0123456789"),
			BankCode:       models.EncryptedString("058"),
			Salary:         money.FromNaira(1000),
			IsActive:       true,
		}).Error)

		itemID := "ITEM-" + uuid.New().String()[:8]
		require.NoError(t, models.DB.Create(&models.PayrollItem{
			ID:             itemID,
			OrganizationID: orgID,
			PayrollID:      payrollID,
			EmployeeID:     empID,
			EmployeeName:   "Worker",
			Amount:         money.FromNaira(1000),
			Status:         models.PayrollProcessing,
		}).Error)
		itemIDs[i] = itemID
	}
	return orgID, payrollID, itemIDs
}

// signedWebhookRequest builds a valid HMAC-signed Monnify webhook for itemID.
// Returns an httptest recorder + gin context wired to the handler input.
func signedWebhookRequest(t *testing.T, itemID, eventType string) (*httptest.ResponseRecorder, *gin.Context) {
	t.Helper()
	body, err := json.Marshal(map[string]interface{}{
		"eventType": eventType,
		"eventData": map[string]interface{}{
			"batchReference":       "irrelevant",
			"transactionReference": itemID,
			"status":               "SUCCESS",
			"amount":               1000,
		},
	})
	require.NoError(t, err)

	mac := hmac.New(sha512.New, []byte(os.Getenv("MONNIFY_SECRET_KEY")))
	mac.Write(body)
	sig := hex.EncodeToString(mac.Sum(nil))

	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/monnify", bytes.NewReader(body))
	req.Header.Set("monnify-signature", sig)
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = req
	return w, c
}

// TestWebhookConcurrency_ExactlyOneReconciliation fires N webhooks for the
// same batch in parallel and asserts:
//
//   - the payroll transitions from processing to completed exactly once
//     (audit log contains exactly one "reconciled" entry)
//   - pending_count lands at zero
//   - every item ends in PayrollCompleted
//
// Pre-Fix #2 this test failed because two webhooks could both observe
// pending_count <= 0 and both fire reconcilePayrollStatus, producing two
// audit entries (and a noisy double FSM error).
func TestWebhookConcurrency_ExactlyOneReconciliation(t *testing.T) {
	skipIfNoDB(t)
	const n = 25
	_, payrollID, itemIDs := seedConcurrentBatch(t, n)

	h := &WebhookHandler{}

	var wg sync.WaitGroup
	wg.Add(n)
	start := make(chan struct{})
	for _, id := range itemIDs {
		go func(itemID string) {
			defer wg.Done()
			<-start
			w, c := signedWebhookRequest(t, itemID, "DISBURSEMENT_SUCCESSFUL")
			h.HandleMonnifyWebhook(c)
			assert.Equal(t, http.StatusOK, w.Code)
		}(id)
	}
	close(start) // unblock all goroutines simultaneously
	wg.Wait()

	// pending_count must be exactly zero (no negative drift from double decrement).
	var pc int
	require.NoError(t, models.DB.Raw(
		"SELECT pending_count FROM payrolls WHERE id = ?", payrollID,
	).Scan(&pc).Error)
	assert.Equal(t, 0, pc, "pending_count must land at exactly 0")

	// Parent payroll must be completed.
	var finalStatus string
	require.NoError(t, models.DB.Raw(
		"SELECT status FROM payrolls WHERE id = ?", payrollID,
	).Scan(&finalStatus).Error)
	assert.Equal(t, string(models.PayrollCompleted), finalStatus)

	// Exactly one reconciliation audit event — the load-bearing assertion.
	var reconCount int64
	require.NoError(t, models.DB.Model(&models.AuditEvent{}).
		Where("entity_type = ? AND entity_id = ? AND action = ?", "Payroll", payrollID, "reconciled").
		Count(&reconCount).Error)
	assert.Equal(t, int64(1), reconCount, "reconciliation must fire exactly once across N concurrent webhooks")

	// Every item must be in terminal completed state.
	var completedItems int64
	require.NoError(t, models.DB.Model(&models.PayrollItem{}).
		Where("payroll_id = ? AND status = ?", payrollID, models.PayrollCompleted).
		Count(&completedItems).Error)
	assert.Equal(t, int64(n), completedItems)
}

// TestWebhookConcurrency_DuplicateRefIsIdempotent fires the same ref many
// times in parallel and asserts the item transitions exactly once. Pre-Fix #2
// the bare-UPDATE FSM would let two webhooks both move the item, audit twice,
// and decrement the parent counter twice.
func TestWebhookConcurrency_DuplicateRefIsIdempotent(t *testing.T) {
	skipIfNoDB(t)
	const dupes = 20
	_, payrollID, itemIDs := seedConcurrentBatch(t, 2)
	target := itemIDs[0]

	h := &WebhookHandler{}

	var wg sync.WaitGroup
	wg.Add(dupes)
	start := make(chan struct{})
	for i := 0; i < dupes; i++ {
		go func() {
			defer wg.Done()
			<-start
			w, c := signedWebhookRequest(t, target, "DISBURSEMENT_SUCCESSFUL")
			h.HandleMonnifyWebhook(c)
			assert.Equal(t, http.StatusOK, w.Code)
		}()
	}
	close(start)
	wg.Wait()

	// Parent pending_count must have decremented at most once for this item.
	// We started at 2, so it should be 1 (the other item is still pending).
	var pc int
	require.NoError(t, models.DB.Raw(
		"SELECT pending_count FROM payrolls WHERE id = ?", payrollID,
	).Scan(&pc).Error)
	assert.Equal(t, 1, pc, "duplicate webhooks for one item must decrement counter exactly once")

	// Only one audit "status_change" entry for that item.
	var auditCount int64
	require.NoError(t, models.DB.Model(&models.AuditEvent{}).
		Where("entity_type = ? AND entity_id = ? AND action = ?", "PayrollItem", target, "status_change").
		Count(&auditCount).Error)
	assert.Equal(t, int64(1), auditCount, "FSM CAS must keep exactly one transition audit entry")
}
