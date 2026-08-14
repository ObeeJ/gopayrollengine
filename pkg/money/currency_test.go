package money

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCurrency_MinorUnitExponents(t *testing.T) {
	// XOF is the one that breaks "everything has cents" assumptions.
	cases := map[Currency]struct {
		exp   int
		scale int64
	}{
		NGN: {2, 100},
		USD: {2, 100},
		GHS: {2, 100},
		KES: {2, 100},
		ZAR: {2, 100},
		GBP: {2, 100},
		EUR: {2, 100},
		XOF: {0, 1},
	}
	for c, want := range cases {
		exp, err := c.Exponent()
		require.NoError(t, err, "currency %s", c)
		assert.Equal(t, want.exp, exp, "exponent for %s", c)

		scale, err := c.Scale()
		require.NoError(t, err, "currency %s", c)
		assert.Equal(t, want.scale, scale, "scale for %s", c)
	}
}

func TestCurrency_UnknownIsRejected(t *testing.T) {
	_, err := ParseCurrency("XYZ")
	require.ErrorIs(t, err, ErrUnknownCurrency)

	_, err = Currency("XYZ").Exponent()
	require.ErrorIs(t, err, ErrUnknownCurrency)

	assert.False(t, Currency("BTC").IsValid())
}

func TestParseCurrency_NormalisesCase(t *testing.T) {
	c, err := ParseCurrency("  ngn ")
	require.NoError(t, err)
	assert.Equal(t, NGN, c)
}

// The invariant the whole type exists to protect: arithmetic never silently
// crosses currencies.
func TestMoney_ArithmeticRefusesCurrencyMismatch(t *testing.T) {
	naira := Money{Minor: 100_00, Currency: NGN}
	dollars := Money{Minor: 100_00, Currency: USD}

	_, err := naira.Add(dollars)
	require.ErrorIs(t, err, ErrCurrencyMismatch)

	_, err = naira.Sub(dollars)
	require.ErrorIs(t, err, ErrCurrencyMismatch)

	_, err = naira.Compare(dollars)
	require.ErrorIs(t, err, ErrCurrencyMismatch,
		"comparing across currencies must error, not return a plausible ordering")
}

func TestMoney_ArithmeticWithinCurrency(t *testing.T) {
	a := Money{Minor: 1_500_00, Currency: NGN}
	b := Money{Minor: 500_00, Currency: NGN}

	sum, err := a.Add(b)
	require.NoError(t, err)
	assert.Equal(t, Money{Minor: 2_000_00, Currency: NGN}, sum)

	diff, err := a.Sub(b)
	require.NoError(t, err)
	assert.Equal(t, Money{Minor: 1_000_00, Currency: NGN}, diff)

	cmp, err := a.Compare(b)
	require.NoError(t, err)
	assert.Equal(t, 1, cmp)
}

func TestMoney_OverflowDetection(t *testing.T) {
	max := Money{Minor: maxInt64, Currency: NGN}
	one := Money{Minor: 1, Currency: NGN}

	_, err := max.Add(one)
	require.ErrorIs(t, err, ErrOverflow)

	min := Money{Minor: minInt64, Currency: NGN}
	_, err = min.Sub(one)
	require.ErrorIs(t, err, ErrOverflow)
}

func TestFromMajor(t *testing.T) {
	m, err := FromMajor(1_500, NGN)
	require.NoError(t, err)
	assert.Equal(t, int64(150_000), m.Minor)

	// XOF has no minor unit — 1500 XOF is 1500 minor units, not 150000.
	x, err := FromMajor(1_500, XOF)
	require.NoError(t, err)
	assert.Equal(t, int64(1_500), x.Minor,
		"a zero-exponent currency must not be scaled by 100")

	_, err = FromMajor(maxInt64/50, NGN)
	require.ErrorIs(t, err, ErrOverflow)
}

func TestParseMoney_ExactDecimalNoFloat(t *testing.T) {
	cases := []struct {
		in   string
		cur  Currency
		want int64
	}{
		{"1500.50", NGN, 150_050},
		{"0.01", USD, 1},
		{"1234567.89", USD, 123_456_789},
		{"-42.42", GBP, -4_242},
		{"1500", XOF, 1_500},
	}
	for _, c := range cases {
		got, err := ParseMoney(c.in, c.cur)
		require.NoError(t, err, "input %q %s", c.in, c.cur)
		assert.Equal(t, c.want, got.Minor, "input %q %s", c.in, c.cur)
		assert.Equal(t, c.cur, got.Currency)
	}
}

// A currency with no minor unit must reject decimal input rather than silently
// truncating it — "1500.50 XOF" is not a real amount.
func TestParseMoney_ZeroExponentRejectsDecimals(t *testing.T) {
	_, err := ParseMoney("1500.50", XOF)
	require.Error(t, err)
}

func TestParseMoney_RejectsTooManyDecimals(t *testing.T) {
	_, err := ParseMoney("10.123", USD)
	require.ErrorIs(t, err, ErrInvalidFormat)
}

func TestParseMoney_RejectsOverflow(t *testing.T) {
	_, err := ParseMoney("92233720368547759", NGN)
	require.ErrorIs(t, err, ErrOverflow)
}

func TestMoney_StringRespectsExponent(t *testing.T) {
	assert.Equal(t, "₦1,500.50", Money{Minor: 150_050, Currency: NGN}.String())
	assert.Equal(t, "$0.01", Money{Minor: 1, Currency: USD}.String())
	assert.Equal(t, "-£42.42", Money{Minor: -4_242, Currency: GBP}.String())
	// No decimal point for a zero-exponent currency.
	assert.Equal(t, "CFA1,500", Money{Minor: 1_500, Currency: XOF}.String())
}

func TestSumMoney_RefusesMixedCurrencies(t *testing.T) {
	_, err := SumMoney([]Money{
		{Minor: 100, Currency: NGN},
		{Minor: 100, Currency: USD},
	}, NGN)
	require.ErrorIs(t, err, ErrCurrencyMismatch)
}

func TestSumMoney_EmptySliceUsesExpectedCurrency(t *testing.T) {
	got, err := SumMoney(nil, USD)
	require.NoError(t, err)
	assert.Equal(t, Money{Minor: 0, Currency: USD}, got)
}

// The bridge between the NGN-only payroll path and the multi-currency ledger
// must not silently accept a non-NGN value.
func TestKoboBridge(t *testing.T) {
	m := NGNFromKobo(Kobo(150_050))
	assert.Equal(t, Money{Minor: 150_050, Currency: NGN}, m)

	back, err := m.Kobo()
	require.NoError(t, err)
	assert.Equal(t, Kobo(150_050), back)

	_, err = Money{Minor: 100, Currency: USD}.Kobo()
	require.ErrorIs(t, err, ErrCurrencyMismatch)
}

func TestCurrency_ScanAndValue(t *testing.T) {
	var c Currency
	require.NoError(t, c.Scan("USD"))
	assert.Equal(t, USD, c)

	require.NoError(t, c.Scan([]byte("NGN")))
	assert.Equal(t, NGN, c)

	require.Error(t, c.Scan("XYZ"))
	require.Error(t, c.Scan(nil), "a NULL currency is never valid")

	v, err := USD.Value()
	require.NoError(t, err)
	assert.Equal(t, "USD", v)

	_, err = Currency("XYZ").Value()
	require.ErrorIs(t, err, ErrUnknownCurrency)
}
