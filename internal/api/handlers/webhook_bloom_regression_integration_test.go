//go:build integration

package handlers

import (
	"net/http"
	"testing"

	"go-payroll-engine/internal/models"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A probabilistic "maybe seen" must never cause a FIRST delivery to be
// acknowledged without being processed. A Bloom filter can only answer
// "definitely new" or "maybe seen" — it has false positives by design, and a
// fixed-size filter that never expires saturates toward 100% false
// positives as references accumulate. Webhooks used to return 200 on "maybe
// seen" without touching the database, so Monnify stopped retrying and the
// disbursement was never recorded as paid or failed. The database-level
// guards (status checks + CAS) are what make redelivery idempotent.
func TestMonnifyWebhook_FirstDeliveryAlwaysProcessed(t *testing.T) {
	skipIfNoDB(t)
	_, itemIDs := seedConcurrentBatch(t, 1)
	itemID := itemIDs[0]

	w, c := signedWebhookRequest(t, itemID, "DISBURSEMENT_SUCCESSFUL")
	(&WebhookHandler{}).HandleMonnifyWebhook(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var item models.PayrollItem
	require.NoError(t, models.DB.First(&item, "id = ?", itemID).Error)
	assert.Equal(t, models.PayrollCompleted, item.Status,
		"a first-time success callback must be recorded, never silently acknowledged")

	// A genuine redelivery is still a clean, idempotent 200.
	w2, c2 := signedWebhookRequest(t, itemID, "DISBURSEMENT_SUCCESSFUL")
	(&WebhookHandler{}).HandleMonnifyWebhook(c2)
	assert.Equal(t, http.StatusOK, w2.Code)
}

// The same guarantee for a failure callback — arguably the worse one to
// drop, since a silently acknowledged DISBURSEMENT_FAILED leaves nobody
// aware the employee was never paid.
func TestMonnifyWebhook_FirstFailureDeliveryAlwaysProcessed(t *testing.T) {
	skipIfNoDB(t)
	_, itemIDs := seedConcurrentBatch(t, 1)

	w, c := signedWebhookRequest(t, itemIDs[0], "DISBURSEMENT_FAILED")
	(&WebhookHandler{}).HandleMonnifyWebhook(c)
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	var item models.PayrollItem
	require.NoError(t, models.DB.First(&item, "id = ?", itemIDs[0]).Error)
	assert.Equal(t, models.PayrollFailed, item.Status)
}
