package models

import (
	"testing"

	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func entry(account string, dir Direction, naira int64) EntryInput {
	return EntryInput{AccountID: account, Direction: dir, Amount: ngn(naira)}
}

// ngn builds a whole-Naira Money for test readability.
func ngn(naira int64) money.Money {
	return money.Money{Minor: naira * 100, Currency: money.NGN}
}

func posting(entries ...EntryInput) PostingRequest {
	return PostingRequest{
		OrgID:          "ORG-1",
		Kind:           "test",
		IdempotencyKey: "key-1",
		Entries:        entries,
	}
}

func TestValidatePosting_BalancedIsAccepted(t *testing.T) {
	err := ValidatePosting(posting(
		entry("receivable", Debit, 50_000),
		entry("cash", Credit, 50_000),
	))
	assert.NoError(t, err)
}

func TestValidatePosting_MultiLineBalanced(t *testing.T) {
	// A draw with a fee: the worker's receivable covers both the cash out and
	// the fee income, and the whole thing still balances.
	err := ValidatePosting(posting(
		entry("receivable", Debit, 51_000),
		entry("cash", Credit, 50_000),
		entry("fee_income", Credit, 1_000),
	))
	assert.NoError(t, err)
}

// The invariant the entire ledger exists to protect.
func TestValidatePosting_UnbalancedIsRejected(t *testing.T) {
	err := ValidatePosting(posting(
		entry("receivable", Debit, 50_000),
		entry("cash", Credit, 49_000),
	))
	require.ErrorIs(t, err, ErrUnbalanced)
}

func TestValidatePosting_RejectsSingleEntry(t *testing.T) {
	err := ValidatePosting(posting(entry("cash", Debit, 50_000)))
	require.ErrorIs(t, err, ErrNoEntries)
}

func TestValidatePosting_RejectsZeroAndNegativeAmounts(t *testing.T) {
	zero := ValidatePosting(posting(
		EntryInput{AccountID: "a", Direction: Debit, Amount: money.Money{Currency: money.NGN}},
		EntryInput{AccountID: "b", Direction: Credit, Amount: money.Money{Currency: money.NGN}},
	))
	require.ErrorIs(t, zero, ErrNonPositiveAmount)

	// A negative debit is an unsigned credit in disguise; direction carries sign.
	negative := ValidatePosting(posting(
		EntryInput{AccountID: "a", Direction: Debit, Amount: ngn(-100)},
		EntryInput{AccountID: "b", Direction: Credit, Amount: ngn(-100)},
	))
	require.ErrorIs(t, negative, ErrNonPositiveAmount)
}

func TestValidatePosting_RequiresIdempotencyKey(t *testing.T) {
	req := posting(
		entry("receivable", Debit, 50_000),
		entry("cash", Credit, 50_000),
	)
	req.IdempotencyKey = ""

	err := ValidatePosting(req)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "idempotency key")
}

func TestValidatePosting_RejectsUnknownDirection(t *testing.T) {
	err := ValidatePosting(posting(
		EntryInput{AccountID: "a", Direction: Direction("sideways"), Amount: ngn(10)},
		entry("b", Credit, 10),
	))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid direction")
}

func TestValidatePosting_DetectsOverflow(t *testing.T) {
	huge := money.Money{Minor: (1 << 62) + (1<<62 - 1), Currency: money.NGN}
	err := ValidatePosting(posting(
		EntryInput{AccountID: "a", Direction: Debit, Amount: huge},
		EntryInput{AccountID: "b", Direction: Debit, Amount: huge},
		EntryInput{AccountID: "c", Direction: Credit, Amount: huge},
		EntryInput{AccountID: "d", Direction: Credit, Amount: huge},
	))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "overflow")
}

func TestNormalBalances_CoverEveryAccountType(t *testing.T) {
	for _, at := range []AccountType{
		AccountEmployerFunding, AccountAdvanceReceivable,
		AccountWagePayable, AccountCashSettlement, AccountFeeIncome,
	} {
		dir, ok := normalBalances[at]
		require.True(t, ok, "account type %s has no normal balance", at)
		assert.Contains(t, []Direction{Debit, Credit}, dir)
	}
}

func TestAdvanceFSM_LegalEdges(t *testing.T) {
	assert.True(t, CanTransitionAdvance(AdvanceRequested, AdvanceApproved))
	assert.True(t, CanTransitionAdvance(AdvanceRequested, AdvanceDeclined))
	assert.True(t, CanTransitionAdvance(AdvanceApproved, AdvanceDisbursed))
	assert.True(t, CanTransitionAdvance(AdvanceDisbursed, AdvanceSettled))
	assert.True(t, CanTransitionAdvance(AdvanceDisbursed, AdvanceWrittenOff))
}

func TestAdvanceFSM_TerminalStatesAreTerminal(t *testing.T) {
	for _, terminal := range []AdvanceStatus{
		AdvanceSettled, AdvanceDeclined, AdvanceCancelled, AdvanceWrittenOff,
	} {
		for _, to := range []AdvanceStatus{
			AdvanceRequested, AdvanceApproved, AdvanceDisbursed, AdvanceSettled,
		} {
			assert.False(t, CanTransitionAdvance(terminal, to),
				"%s must be terminal, but %s → %s was allowed", terminal, terminal, to)
		}
	}
}

// A settled advance must never be re-settled: that would credit the receivable
// twice and make the worker's debt appear paid off when only half of it was.
func TestAdvanceFSM_NoDoubleSettlement(t *testing.T) {
	assert.False(t, CanTransitionAdvance(AdvanceSettled, AdvanceSettled))
	assert.False(t, CanTransitionAdvance(AdvanceRequested, AdvanceSettled),
		"settlement must go through approval and disbursement")
}

// The invariant migration 000015 exists to protect: a transaction that mixes
// currencies must be rejected before any arithmetic is attempted. Summing
// ₦50,000 against $50,000 produces a "balanced" transaction and silent money
// corruption.
func TestValidatePosting_RejectsMixedCurrencies(t *testing.T) {
	err := ValidatePosting(posting(
		EntryInput{AccountID: "a", Direction: Debit, Amount: money.Money{Minor: 50_000_00, Currency: money.NGN}},
		EntryInput{AccountID: "b", Direction: Credit, Amount: money.Money{Minor: 50_000_00, Currency: money.USD}},
	))
	require.ErrorIs(t, err, money.ErrCurrencyMismatch)
}

func TestValidatePosting_AcceptsNonNairaSingleCurrency(t *testing.T) {
	err := ValidatePosting(posting(
		EntryInput{AccountID: "a", Direction: Debit, Amount: money.Money{Minor: 25_00, Currency: money.USD}},
		EntryInput{AccountID: "b", Direction: Credit, Amount: money.Money{Minor: 25_00, Currency: money.USD}},
	))
	assert.NoError(t, err, "the ledger must not be NGN-only")
}

func TestValidatePosting_RejectsUnknownCurrency(t *testing.T) {
	err := ValidatePosting(posting(
		EntryInput{AccountID: "a", Direction: Debit, Amount: money.Money{Minor: 100, Currency: money.Currency("XYZ")}},
		EntryInput{AccountID: "b", Direction: Credit, Amount: money.Money{Minor: 100, Currency: money.Currency("XYZ")}},
	))
	require.ErrorIs(t, err, money.ErrUnknownCurrency)
}
