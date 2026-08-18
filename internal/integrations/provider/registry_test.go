package provider

import (
	"context"
	"testing"

	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeProvider is a minimal Provider for exercising the registry without any
// real network client.
type fakeProvider struct {
	name       string
	currencies map[money.Currency]bool
}

func (f *fakeProvider) Name() string { return f.name }
func (f *fakeProvider) SupportsCurrency(c money.Currency) bool {
	return f.currencies[c]
}
func (f *fakeProvider) InitiateTransfer(context.Context, TransferRequest) (TransferResult, error) {
	return TransferResult{Accepted: true}, nil
}
func (f *fakeProvider) GetTransferStatus(context.Context, string) (TransferStatus, error) {
	return StatusPending, nil
}

func TestRegistry_SelectsByPriorityOrder(t *testing.T) {
	a := &fakeProvider{name: "a", currencies: map[money.Currency]bool{money.NGN: true}}
	b := &fakeProvider{name: "b", currencies: map[money.Currency]bool{money.NGN: true, money.USD: true}}
	reg := NewRegistry(a, b)

	got, err := reg.Select(money.NGN)
	require.NoError(t, err)
	assert.Equal(t, "a", got.Name(), "the first-registered provider that supports the currency must win")

	got, err = reg.Select(money.USD)
	require.NoError(t, err)
	assert.Equal(t, "b", got.Name(), "must fall through to the next provider when the first doesn't support the currency")
}

func TestRegistry_NoProviderForCurrency(t *testing.T) {
	reg := NewRegistry(&fakeProvider{name: "a", currencies: map[money.Currency]bool{money.NGN: true}})

	_, err := reg.Select(money.EUR)
	require.ErrorIs(t, err, ErrNoProviderForCurrency,
		"an unroutable currency must fail loudly, not silently fall back to a provider that cannot settle it")
}

func TestRegistry_ByName(t *testing.T) {
	a := &fakeProvider{name: "monnify", currencies: map[money.Currency]bool{money.NGN: true}}
	b := &fakeProvider{name: "paystack", currencies: map[money.Currency]bool{money.NGN: true}}
	reg := NewRegistry(a, b)

	got, err := reg.ByName("paystack")
	require.NoError(t, err)
	assert.Same(t, b, got)

	_, err = reg.ByName("flutterwave")
	require.Error(t, err)
}

func TestRegistry_EmptyRegistryAlwaysFails(t *testing.T) {
	reg := NewRegistry()
	_, err := reg.Select(money.NGN)
	require.ErrorIs(t, err, ErrNoProviderForCurrency)
}
