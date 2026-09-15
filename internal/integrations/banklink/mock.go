package banklink

import (
	"context"
	"fmt"
	"time"

	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
)

// Mock is a deterministic, no-network Provider (and DebitProvider) for
// tests and local development (MOCK_MODE=true, mirroring monnify.Client and
// paystack.Client's own convention). It never calls out anywhere; a real
// aggregator adapter wraps an actual HTTP client the same way monnify.Client
// does.
//
// Transactions is keyed by providerAccountRef and returned verbatim by
// GetTransactions — tests set it up directly rather than driving it through
// InitiateLink/CompleteLink, since those two just fabricate a plausible
// session/account and carry nothing a test would want to control.
type Mock struct {
	Transactions map[string][]Transaction

	// DebitOutcome is what GetDebitStatus reports for any provider reference
	// InitiateDebit issued — defaults to DebitStatusSuccessful. Set it
	// before a test's InitiateDebit call to simulate a failed or unresolved
	// collection.
	DebitOutcome DebitStatus

	// InitiateDebitError, if set, is returned by InitiateDebit instead of a
	// result — simulates ErrDebitUnavailable or similar.
	InitiateDebitError error

	mandates map[string]string // providerAccountRef -> mandateRef
	debits   []DebitCall
}

// DebitCall records one InitiateDebit invocation, for tests that assert on
// what was actually submitted rather than just the outcome.
type DebitCall struct {
	MandateRef string
	Amount     money.Money
	Reference  string
}

// NewMock builds an empty Mock; set .Transactions before calling
// GetTransactions in a test.
func NewMock() *Mock {
	return &Mock{
		Transactions: map[string][]Transaction{},
		DebitOutcome: DebitStatusSuccessful,
		mandates:     map[string]string{},
	}
}

// Debits returns every InitiateDebit call this Mock has seen, oldest first.
func (m *Mock) Debits() []DebitCall {
	return m.debits
}

func (m *Mock) Name() string { return "mock" }

func (m *Mock) InitiateLink(ctx context.Context, reference string) (LinkSession, error) {
	return LinkSession{
		SessionToken: "mock-session-" + uuid.New().String()[:8],
		RedirectURL:  "https://mock.banklink.local/link/" + reference,
	}, nil
}

func (m *Mock) CompleteLink(ctx context.Context, callbackToken string) (LinkedAccount, error) {
	return LinkedAccount{
		ProviderAccountRef: "mock-acct-" + uuid.New().String()[:8],
		AccountName:        "Mock Test Account",
		BankName:           "Mock Bank",
	}, nil
}

func (m *Mock) GetTransactions(ctx context.Context, providerAccountRef string, since time.Time) ([]Transaction, error) {
	all, ok := m.Transactions[providerAccountRef]
	if !ok {
		return nil, fmt.Errorf("banklink mock: no transactions seeded for %q", providerAccountRef)
	}
	filtered := make([]Transaction, 0, len(all))
	for _, tx := range all {
		if !tx.Date.Before(since) {
			filtered = append(filtered, tx)
		}
	}
	return filtered, nil
}

func (m *Mock) AuthorizeDebitMandate(ctx context.Context, providerAccountRef string) (string, error) {
	mandateRef := "mock-mandate-" + uuid.New().String()[:8]
	m.mandates[mandateRef] = providerAccountRef
	return mandateRef, nil
}

func (m *Mock) InitiateDebit(ctx context.Context, mandateRef string, amount money.Money, reference string) (DebitResult, error) {
	if m.InitiateDebitError != nil {
		return DebitResult{}, m.InitiateDebitError
	}
	if _, ok := m.mandates[mandateRef]; !ok {
		return DebitResult{}, ErrNoMandate
	}
	m.debits = append(m.debits, DebitCall{MandateRef: mandateRef, Amount: amount, Reference: reference})
	return DebitResult{
		Accepted:          true,
		ProviderReference: "mock-debit-" + uuid.New().String()[:8],
		Message:           "accepted",
	}, nil
}

func (m *Mock) GetDebitStatus(ctx context.Context, providerReference string) (DebitStatus, error) {
	return m.DebitOutcome, nil
}
