// Package provider defines the payment-service-provider boundary: the shape
// every disbursement rail (Monnify, Paystack, Flutterwave, ...) must satisfy so
// the rest of the system can move money without knowing which bank rail is
// underneath.
//
// This exists because the engine was hard-wired to a single provider
// (Monnify), which meant three things could not happen without a rewrite:
// switching providers, running two providers side by side for redundancy, and
// paying out in a currency Monnify does not settle. None of those are
// hypothetical — Monnify is NGN-only, and an outage on a single rail with no
// fallback is a single point of failure in a system that moves salaries.
//
// A provider here only ever SUBMITS a transfer and reports what the provider
// said synchronously. Final confirmation — money actually landed or was
// rejected — arrives later via that provider's webhook. Modelling it any other
// way would claim a certainty the rail itself does not give: bank transfers in
// every market this targets are asynchronous by nature.
package provider

import (
	"context"
	"errors"

	"go-payroll-engine/pkg/money"
)

// TransferStatus is the provider's own account of a transfer, as of the moment
// asked. It is intentionally coarser than the FSM in models.AdvanceStatus —
// callers translate this into their own state machine rather than importing
// the provider's vocabulary into the domain.
type TransferStatus string

const (
	// StatusPending — accepted by the rail, outcome not yet known.
	StatusPending TransferStatus = "pending"
	// StatusSuccessful — the rail confirms funds arrived.
	StatusSuccessful TransferStatus = "successful"
	// StatusFailed — the rail confirms the transfer did not complete. Money
	// that was provisionally held is expected to be released by the provider.
	StatusFailed TransferStatus = "failed"
	// StatusUnknown — the provider could not be reached or gave an
	// unrecognised answer. Callers must not treat this as failure: retry or
	// wait for the webhook rather than reversing money on an unknown.
	StatusUnknown TransferStatus = "unknown"
)

// TransferRequest is one payout: a single named recipient, one amount, one
// currency. Deliberately not a batch — batching is a provider-side optimise,
// not a domain concept, and forcing every provider through a single-item
// shape keeps EWA (always one recipient) and payroll (many) using the same
// interface without payroll's fan-out leaking into EWA's code path.
type TransferRequest struct {
	// Reference is OUR identifier for this transfer — an advance ID or payroll
	// item ID. Also used as the provider-side idempotency key wherever the
	// provider supports one, so a retried submission cannot double-pay.
	Reference string
	Amount    money.Money

	RecipientName          string
	RecipientAccountNumber string
	RecipientBankCode      string

	// Narration appears on the recipient's bank statement.
	Narration string
}

// TransferResult is what the provider said at submission time — accepted or
// rejected, never "succeeded". A rejection here is synchronous and final (bad
// account number, insufficient balance); an acceptance still requires webhook
// confirmation before the money can be treated as moved.
type TransferResult struct {
	Accepted bool
	// ProviderReference is the provider's own identifier for this transfer,
	// distinct from Reference — needed to correlate a later webhook or a
	// GetTransferStatus call back to this submission when the provider issues
	// its own reference rather than echoing ours.
	ProviderReference string
	// Message is the provider's human-readable response, kept for audit and
	// for surfacing "insufficient balance"-type reasons without inventing our
	// own taxonomy of every provider's error codes.
	Message string
}

// Errors every provider implementation returns for the same failure shape, so
// callers can branch on error identity instead of inspecting provider-specific
// strings.
var (
	// ErrCurrencyUnsupported — asked to pay out in a currency this provider
	// does not settle. Registry.Select filters on this before calling in, so
	// a provider seeing this from its own InitiateTransfer indicates a
	// registry bug, not routine control flow.
	ErrCurrencyUnsupported = errors.New("provider: currency not supported")
	// ErrProviderUnavailable — the rail could not be reached (network,
	// authentication, 5xx). Distinguished from a business rejection because
	// callers retry this and must NOT reverse any provisional ledger state on
	// it — the transfer may have gone through even though the response did not
	// arrive.
	ErrProviderUnavailable = errors.New("provider: unavailable")
)

// Provider is the full contract a payment rail must satisfy to be routed
// disbursements. Implementations live under internal/integrations/<name> and
// wrap that provider's own client so this interface stays free of any single
// rail's wire format.
type Provider interface {
	// Name identifies the provider in logs, metrics, and the persisted
	// provider_name column — must be stable, since past rows depend on it.
	Name() string

	// SupportsCurrency reports whether this provider can settle payouts in c.
	// The registry uses this to route; a provider must not silently accept a
	// currency it cannot actually pay out.
	SupportsCurrency(c money.Currency) bool

	// InitiateTransfer submits one payout. Returns ErrProviderUnavailable on
	// any failure to reach the rail or interpret its response — the caller's
	// job is to retry that, never to assume failure from it. A synchronous
	// business rejection (bad account, insufficient funds) is NOT an error:
	// it is a TransferResult with Accepted=false, because the rail was reached
	// and gave a definite answer.
	InitiateTransfer(ctx context.Context, req TransferRequest) (TransferResult, error)

	// GetTransferStatus polls the rail for a transfer's current status, keyed
	// by the ProviderReference returned from InitiateTransfer. Used as a
	// fallback reconciliation path when a webhook is late or lost — never as
	// the primary confirmation mechanism, since polling every transfer would
	// not scale and webhooks are the rail's own authoritative push channel.
	GetTransferStatus(ctx context.Context, providerReference string) (TransferStatus, error)
}
