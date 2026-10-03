package banklink

import (
	"context"
	"testing"
	"time"

	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A freshly linked account — via the real CompleteLink flow, not a test
// pre-seeding Transactions — has no entry in the map at all. GetTransactions
// must treat that as "no history yet", the same as any other real worker
// this Mock has never been told about a deposit for, not as a fault.
func TestGetTransactions_UnseededAccountReturnsEmptyNotError(t *testing.T) {
	m := NewMock()

	txs, err := m.GetTransactions(context.Background(), "mock-acct-never-seeded", time.Now().AddDate(0, -6, 0))

	require.NoError(t, err)
	assert.Empty(t, txs)
}

func TestGetTransactions_SeededAccountFiltersBySince(t *testing.T) {
	m := NewMock()
	now := time.Now()
	m.Transactions["mock-acct-1"] = []Transaction{
		{Date: now.AddDate(0, 0, -100), Amount: money.FromNaira(50_000), Direction: Credit},
		{Date: now.AddDate(0, 0, -10), Amount: money.FromNaira(300_000), Direction: Credit},
	}

	txs, err := m.GetTransactions(context.Background(), "mock-acct-1", now.AddDate(0, 0, -30))

	require.NoError(t, err)
	require.Len(t, txs, 1)
	assert.Equal(t, money.FromNaira(300_000), txs[0].Amount)
}

// The demo mock exists so a D2C worker can be taken end to end under
// MOCK_MODE: it invents a plausible salary history, and keeps no per-process
// state, because the API (which authorises the mandate) and the collection
// sweep (which debits it) are different processes.
func TestDemoMock_InventsASalaryHistoryOnlyForMockAccounts(t *testing.T) {
	m := NewDemoMock()
	now := time.Now()

	txs, err := m.GetTransactions(context.Background(), "mock-acct-abc", now.AddDate(0, -6, 0))
	require.NoError(t, err)
	require.Len(t, txs, 4)
	for _, tx := range txs {
		assert.Equal(t, Credit, tx.Direction)
		assert.Equal(t, money.FromNaira(250_000), tx.Amount)
		assert.False(t, tx.Date.After(now))
	}

	other, err := m.GetTransactions(context.Background(), "real-looking-ref", now.AddDate(0, -6, 0))
	require.NoError(t, err)
	assert.Empty(t, other, "only mock accounts get invented income")

	m.Transactions["mock-acct-seeded"] = []Transaction{{Date: now.AddDate(0, 0, -3), Amount: money.FromNaira(10), Direction: Credit}}
	seeded, err := m.GetTransactions(context.Background(), "mock-acct-seeded", now.AddDate(0, -6, 0))
	require.NoError(t, err)
	assert.Len(t, seeded, 1, "explicitly seeded history wins over the invented one")
}

func TestDemoMock_MandatesSurviveAcrossInstances(t *testing.T) {
	ref, err := NewDemoMock().AuthorizeDebitMandate(context.Background(), "mock-acct-abc")
	require.NoError(t, err)

	// A different instance, as in a different process.
	res, err := NewDemoMock().InitiateDebit(context.Background(), ref, ngn5000(t), "COLL-1")
	require.NoError(t, err)
	assert.True(t, res.Accepted)

	_, err = NewDemoMock().InitiateDebit(context.Background(), "not-a-mandate", ngn5000(t), "COLL-2")
	assert.ErrorIs(t, err, ErrNoMandate)
}

func TestPlainMock_StillRejectsMandatesItDidNotIssue(t *testing.T) {
	_, err := NewMock().InitiateDebit(context.Background(), "mock-mandate-from-elsewhere", ngn5000(t), "COLL-3")
	assert.ErrorIs(t, err, ErrNoMandate, "the strict mock used by unit tests keeps its per-instance behaviour")
}

func ngn5000(t *testing.T) money.Money {
	t.Helper()
	m, err := money.New(5_000_00, money.NGN)
	require.NoError(t, err)
	return m
}
