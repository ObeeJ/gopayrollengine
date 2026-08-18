package provider

import (
	"fmt"

	"go-payroll-engine/pkg/money"
)

// ErrNoProviderForCurrency is returned when nothing registered can settle the
// requested currency. A payout request that hits this must fail loudly and
// early — silently falling back to some other currency's provider would move
// money through a rail that cannot actually pay it out.
var ErrNoProviderForCurrency = fmt.Errorf("provider: no provider configured for this currency")

// Registry selects a Provider for a currency, in priority order. Priority is
// registration order: the first provider that supports a currency wins,
// letting an operator make provider B the fallback for provider A's currencies
// simply by registering it second — no per-currency configuration required
// until there is an actual second provider to choose between.
//
// This is deliberately currency-scoped rather than org-scoped. Per-organisation
// provider preference (an org configuring "always use Paystack") is a real
// future need but a distinct piece of product surface — it needs its own
// config table, admin UI, and migration path for orgs that already have
// in-flight transfers with a provider. Building that speculatively here would
// be exactly the kind of premature abstraction this codebase avoids elsewhere;
// currency-based routing is what today's callers actually need.
type Registry struct {
	providers []Provider
}

// NewRegistry builds a registry from providers in priority order.
func NewRegistry(providers ...Provider) *Registry {
	return &Registry{providers: providers}
}

// Select returns the highest-priority provider that supports currency c.
func (r *Registry) Select(c money.Currency) (Provider, error) {
	for _, p := range r.providers {
		if p.SupportsCurrency(c) {
			return p, nil
		}
	}
	return nil, fmt.Errorf("%w: %s", ErrNoProviderForCurrency, c)
}

// ByName returns the provider with the given Name(), for reconciliation code
// that already knows which provider handled a transfer (e.g. from the
// provider_name column) and needs that exact instance rather than routing
// freshly by currency.
func (r *Registry) ByName(name string) (Provider, error) {
	for _, p := range r.providers {
		if p.Name() == name {
			return p, nil
		}
	}
	return nil, fmt.Errorf("provider: no provider registered with name %q", name)
}
