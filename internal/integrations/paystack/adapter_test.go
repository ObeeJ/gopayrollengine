package paystack

import (
	"context"
	"testing"

	"go-payroll-engine/internal/integrations/provider"
	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAdapter_SatisfiesProviderInterface(t *testing.T) {
	var _ provider.Provider = (*Adapter)(nil)
}

func TestAdapter_SupportsOnlyNGN(t *testing.T) {
	a := NewAdapter(&Client{})
	assert.True(t, a.SupportsCurrency(money.NGN))
	assert.False(t, a.SupportsCurrency(money.GHS),
		"GHS payout is not enabled until the recipient type mapping is verified")
}

func TestAdapter_RejectsUnsupportedCurrency(t *testing.T) {
	a := NewAdapter(&Client{MockMode: true})
	_, err := a.InitiateTransfer(context.Background(), provider.TransferRequest{
		Reference: "TEST-1",
		Amount:    money.Money{Minor: 1000, Currency: money.EUR},
	})
	require.ErrorIs(t, err, provider.ErrCurrencyUnsupported)
}

func TestAdapter_MockModeAccepts(t *testing.T) {
	a := NewAdapter(&Client{MockMode: true})
	result, err := a.InitiateTransfer(context.Background(), provider.TransferRequest{
		Reference:              "TEST-2",
		Amount:                 money.Money{Minor: 150_000, Currency: money.NGN},
		RecipientName:          "Ada",
		RecipientAccountNumber: "0123456789",
		RecipientBankCode:      "058",
	})
	require.NoError(t, err)
	assert.True(t, result.Accepted)
	assert.Equal(t, "TRF_mock_TEST-2", result.ProviderReference)
}

func TestAdapter_GetTransferStatus_MapsPaystackStatuses(t *testing.T) {
	// MockMode's VerifyTransfer always reports "success" — this test documents
	// the mapping rather than exercising every branch, since the real values
	// only ever arrive from a live or recorded response.
	a := NewAdapter(&Client{MockMode: true})
	status, err := a.GetTransferStatus(context.Background(), "TRF_mock_TEST-2")
	require.NoError(t, err)
	assert.Equal(t, provider.StatusSuccessful, status)
}
