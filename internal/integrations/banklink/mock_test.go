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
