package banklink

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Mock is a deterministic, no-network Provider for tests and local
// development (MOCK_MODE=true, mirroring monnify.Client and
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
}

// NewMock builds an empty Mock; set .Transactions before calling
// GetTransactions in a test.
func NewMock() *Mock {
	return &Mock{Transactions: map[string][]Transaction{}}
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
