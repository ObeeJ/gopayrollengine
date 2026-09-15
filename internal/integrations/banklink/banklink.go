// Package banklink defines the account-linking boundary for direct-to-
// consumer EWA: the shape any open-banking aggregator (Mono, Okra, ...) must
// satisfy so the rest of the system can read a worker's own deposit history
// without knowing which aggregator is underneath.
//
// This exists because D2C has no payroll relationship to draw eligibility
// or recourse from (see migration 000030's comment) — a worker's income has
// to be observed from their own bank account instead, and repayment has to
// be collected from it directly rather than netted out of a payroll item.
// Read access (GetTransactions, for payday prediction) and money movement
// (a future debit capability) are deliberately split at this interface
// boundary and not yet unified: this package only ever reads, on purpose,
// so linking an account for eligibility can never accidentally imply
// authorization to debit it.
//
// No concrete adapter for a real aggregator ships here yet — see Mock for
// the MOCK_MODE-capable stand-in tests and local development use. Wiring an
// actual provider (Mono/Okra's real API) is separate follow-up work; this
// package is the contract that work has to satisfy.
package banklink

import (
	"context"
	"time"

	"go-payroll-engine/pkg/money"
)

// TransactionDirection — which way money moved on a linked account.
type TransactionDirection string

const (
	Credit TransactionDirection = "credit"
	Debit  TransactionDirection = "debit"
)

// Transaction is one entry from a linked account's history, the minimum a
// payday-prediction algorithm needs and nothing more — no account number,
// no counterparty details, no balance.
type Transaction struct {
	Date      time.Time
	Amount    money.Kobo
	Direction TransactionDirection
	Narration string
}

// LinkSession is what InitiateLink hands back — a URL or token the client
// redirects the consumer to, to complete linking on the aggregator's own UI.
// Deliberately opaque here: different aggregators use different flows
// (hosted redirect, widget token, ...) and this type doesn't try to unify
// their shapes, only carry whichever one this provider uses.
type LinkSession struct {
	SessionToken string
	RedirectURL  string
}

// LinkedAccount is what CompleteLink hands back once a consumer finishes
// linking — durable enough to store (see d2c_bank_links.provider_account_ref)
// and re-request transactions against later.
type LinkedAccount struct {
	ProviderAccountRef string
	AccountName        string
	BankName            string
}

// Provider is the account-linking contract a real aggregator must satisfy.
// Implementations live under internal/integrations/<name>.
type Provider interface {
	// Name identifies the provider in the persisted provider column — must
	// be stable, since past d2c_bank_links rows depend on it.
	Name() string

	// InitiateLink starts a link flow for one consumer. reference is our own
	// identifier (an employee ID), passed through so the provider's
	// callback can be correlated back to the right worker.
	InitiateLink(ctx context.Context, reference string) (LinkSession, error)

	// CompleteLink exchanges a provider callback token for the durable
	// account reference to store. Called once, when the consumer finishes
	// the provider's own linking flow.
	CompleteLink(ctx context.Context, callbackToken string) (LinkedAccount, error)

	// GetTransactions returns transaction history for a linked account since
	// the given time — read-only, the only capability this package exposes.
	GetTransactions(ctx context.Context, providerAccountRef string, since time.Time) ([]Transaction, error)
}
