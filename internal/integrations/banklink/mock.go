package banklink

import (
	"context"
	"strings"
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
// session/account and carry nothing a test would want to control. An
// account with no entry returns zero transactions, not an error — a real
// aggregator gives exactly that for any account it has genuinely never
// observed a transaction on (a freshly linked one, most obviously), and a
// caller that treated "no data yet" as a hard failure would 500 every real
// worker's first eligibility check right after linking. See
// TestGetTransactions_UnseededAccountReturnsEmptyNotError.
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

	// Demo (see NewDemoMock) makes the mock usable across processes and gives
	// mock accounts an income history, so MOCK_MODE can show a D2C worker an
	// advance, its repayment debit and the debit callback end to end.
	Demo bool

	mandates map[string]string // providerAccountRef -> mandateRef
	debits   []DebitCall
}

const (
	mockMandatePrefix = "mock-mandate-"
	mockAccountPrefix = "mock-acct-"
)

// NewDemoMock is the MOCK_MODE wiring (routes.go and the collection sweep).
// Unlike NewMock, which unit tests rely on for strict per-instance behaviour:
//   - a mock account with no seeded history shows four monthly ₦250,000
//     salary credits ending ten days ago (so a payday is predictable);
//   - any mandate reference this mock could have issued is honoured, because
//     the API process authorises the mandate and a separate sweep process
//     debits it, and neither shares memory with the other.
func NewDemoMock() *Mock {
	m := NewMock()
	m.Demo = true
	return m
}

func (m *Mock) syntheticIncome() []Transaction {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	out := make([]Transaction, 0, 4)
	for i := 0; i < 4; i++ {
		out = append(out, Transaction{
			Date:      today.AddDate(0, 0, -10-30*i),
			Amount:    money.FromNaira(250_000),
			Direction: Credit,
			Narration: "SALARY (demo)",
		})
	}
	return out
}

// DebitCall records one InitiateDebit invocation, for tests that assert on
// what was actually submitted rather than just the outcome.
type DebitCall struct {
	MandateRef string
	Amount     money.Money
	Reference  string
}

// NewMock builds an empty Mock. Set .Transactions before calling
// GetTransactions in a test that needs specific history; an unset account
// simply has none.
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
		ProviderAccountRef: mockAccountPrefix + uuid.New().String()[:8],
		AccountName:        "Mock Test Account",
		BankName:           "Mock Bank",
	}, nil
}

func (m *Mock) GetTransactions(ctx context.Context, providerAccountRef string, since time.Time) ([]Transaction, error) {
	all, seeded := m.Transactions[providerAccountRef]
	if !seeded && m.Demo && strings.HasPrefix(providerAccountRef, mockAccountPrefix) {
		all = m.syntheticIncome()
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
	mandateRef := mockMandatePrefix + uuid.New().String()[:8]
	m.mandates[mandateRef] = providerAccountRef
	return mandateRef, nil
}

func (m *Mock) InitiateDebit(ctx context.Context, mandateRef string, amount money.Money, reference string) (DebitResult, error) {
	if m.InitiateDebitError != nil {
		return DebitResult{}, m.InitiateDebitError
	}
	_, issuedHere := m.mandates[mandateRef]
	issuedElsewhere := m.Demo && strings.HasPrefix(mandateRef, mockMandatePrefix)
	if !issuedHere && !issuedElsewhere {
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
