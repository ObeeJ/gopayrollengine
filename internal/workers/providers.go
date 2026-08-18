package workers

import (
	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/integrations/paystack"
	"go-payroll-engine/internal/integrations/provider"
)

// DefaultProviderRegistry builds the priority-ordered provider registry from
// env-configured clients. Monnify first — the existing, already-integrated NGN
// rail — Paystack second as an alternate/fallback for the same currency. See
// provider.Registry's doc comment for why priority here is plain registration
// order rather than per-org configuration: that is real future product
// surface, not something to build ahead of an actual second currency or a
// customer who has asked to choose their own rail.
func DefaultProviderRegistry() *provider.Registry {
	return provider.NewRegistry(
		monnify.NewAdapter(monnify.NewClient()),
		paystack.NewAdapter(paystack.NewClient()),
	)
}
