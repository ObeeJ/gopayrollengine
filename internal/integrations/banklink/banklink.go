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
	"errors"
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
	BankName           string
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

// DebitStatus is a debit's own account of itself, as of the moment asked —
// deliberately the same shape as provider.TransferStatus, since a debit is
// asynchronous for exactly the same reason a payout is: initiating one only
// means the rail accepted the request, not that funds actually moved.
// Insufficient funds, a since-revoked mandate, or a closed account are all
// ordinary outcomes here, not exceptional ones — ConfirmD2CCollection (the
// caller on the settlement side) must wait for this, never infer success
// from acceptance alone.
type DebitStatus string

const (
	DebitStatusPending    DebitStatus = "pending"
	DebitStatusSuccessful DebitStatus = "successful"
	DebitStatusFailed     DebitStatus = "failed"
	DebitStatusUnknown    DebitStatus = "unknown"
)

// DebitResult is what a provider says at submission time — mirrors
// provider.TransferResult's own posture: Accepted is a synchronous, definite
// answer (bad mandate, no such account); a true debit outcome (funds moved
// or didn't) arrives later via webhook or GetDebitStatus.
type DebitResult struct {
	Accepted          bool
	ProviderReference string
	Message           string
}

// Errors a DebitProvider implementation returns for the same failure shape,
// mirroring package provider's own error identities so callers can branch
// on error identity rather than provider-specific strings.
var (
	// ErrDebitUnavailable — the rail could not be reached. Callers retry
	// this; it says nothing about whether the debit went through.
	ErrDebitUnavailable = errors.New("banklink: debit rail unavailable")
	// ErrNoMandate — asked to debit an account with no authorized mandate.
	// A caller seeing this from InitiateDebit indicates it skipped
	// AuthorizeDebitMandate, not routine control flow.
	ErrNoMandate = errors.New("banklink: no debit mandate authorized for this account")
)

// DebitProvider is the additional capability a Provider may offer: pulling
// money FROM a linked account under a standing authorization. Kept as a
// separate interface from Provider — embedding it rather than folding these
// methods into Provider itself — so that implementing account linking never
// implies debit capability by accident; a caller that only needs to read
// transaction history for payday prediction should never be handed
// something that can also move money, even if the concrete adapter happens
// to support both.
type DebitProvider interface {
	Provider

	// AuthorizeDebitMandate sets up standing authorization to debit a
	// linked account later, on the worker's own predicted payday. A
	// separate, explicit step from linking the account for reading — see
	// migration 000030 and 000031's comments on why consent to read and
	// consent to be debited must never be the same consent.
	AuthorizeDebitMandate(ctx context.Context, providerAccountRef string) (mandateRef string, err error)

	// InitiateDebit pulls amount from an account under an existing mandate.
	// Reference is our own identifier (a d2c_debit_collections row ID),
	// used the same way provider.TransferRequest.Reference is: as the
	// provider-side idempotency key wherever the rail supports one, so a
	// retried submission cannot double-debit.
	InitiateDebit(ctx context.Context, mandateRef string, amount money.Money, reference string) (DebitResult, error)

	// GetDebitStatus polls the rail for a debit's current status, keyed by
	// the ProviderReference InitiateDebit returned. A fallback path when a
	// webhook is late or lost — never the primary confirmation mechanism.
	GetDebitStatus(ctx context.Context, providerReference string) (DebitStatus, error)
}
