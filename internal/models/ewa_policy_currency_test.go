package models

import (
	"testing"

	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
)

// DefaultEWAPolicy must never hand a non-NGN org Naira-labelled amounts
// reinterpreted as if they were its own currency — that would be off by
// whatever the two currencies' relative value happens to be, silently.
func TestDefaultEWAPolicy_UsesTheOrgsOwnCurrency(t *testing.T) {
	ngn := DefaultEWAPolicy("ORG-1", money.NGN)
	ghs := DefaultEWAPolicy("ORG-1", money.GHS)

	assert.Equal(t, money.FromNaira(200_000), ngn.AbsoluteCapKobo)
	assert.NotEqual(t, ngn.AbsoluteCapKobo, ghs.AbsoluteCapKobo,
		"a GHS org must not get the NGN org's raw minor-unit cap")
	assert.True(t, ghs.AbsoluteCapKobo.IsPositive())
	assert.True(t, ghs.MinDrawKobo.IsPositive())
	assert.True(t, ghs.EmergencyFloorKobo.IsPositive())
}

// Every currency this platform recognises must have an entry, or an org in
// that currency would silently fall back to NGN-shaped defaults.
func TestDefaultEWAPolicy_CoversEveryPlatformCurrency(t *testing.T) {
	for _, c := range []money.Currency{money.NGN, money.USD, money.GHS, money.KES, money.ZAR, money.XOF, money.GBP, money.EUR} {
		policy := DefaultEWAPolicy("ORG-1", c)
		assert.Truef(t, policy.AbsoluteCapKobo.IsPositive(), "%s: absolute cap must be positive", c)
		assert.Truef(t, policy.MinDrawKobo.IsPositive(), "%s: min draw must be positive", c)
		assert.Truef(t, policy.EmergencyFloorKobo.IsPositive(), "%s: emergency floor must be positive", c)
		assert.Truef(t, policy.MinDrawKobo < policy.AbsoluteCapKobo, "%s: min draw must be below the cap", c)
	}
}

// An unrecognised currency (should never happen — the DB CHECK constraint on
// organizations.currency prevents it) falls back to NGN's shape rather than
// zero, so TierCap's "never zero" invariant still holds even under a defect
// elsewhere.
func TestDefaultEWAPolicy_UnknownCurrencyFallsBackToNGNShape(t *testing.T) {
	policy := DefaultEWAPolicy("ORG-1", money.Currency("XYZ"))
	assert.Equal(t, money.FromNaira(200_000), policy.AbsoluteCapKobo)
}
