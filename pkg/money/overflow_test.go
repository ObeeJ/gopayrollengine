package money

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FromNairaString is reachable from request bodies through Kobo.UnmarshalJSON,
// so a wrapped multiply here is attacker-controlled: a large enough whole-Naira
// string used to wrap into a negative Kobo amount.
func TestFromNairaString_RejectsOverflow(t *testing.T) {
	// 92233720368547759 × 100 exceeds MaxInt64.
	_, err := FromNairaString("92233720368547759")
	require.ErrorIs(t, err, ErrOverflow)
}

func TestFromNairaString_AcceptsLargestSafeValue(t *testing.T) {
	got, err := FromNairaString("92233720368547758")
	require.NoError(t, err)
	assert.Equal(t, Kobo(9223372036854775800), got)
}

func TestUnmarshalJSON_OverflowStringIsRejected(t *testing.T) {
	var k Kobo
	err := json.Unmarshal([]byte(`"92233720368547759"`), &k)
	require.Error(t, err, "an overflowing amount must not silently wrap negative")
	assert.Equal(t, Kobo(0), k)
}

func TestFromNairaString_StillParsesOrdinaryAmounts(t *testing.T) {
	cases := map[string]Kobo{
		"0":        0,
		"1500":     150000,
		"1500.50":  150050,
		"1500.5":   150050,
		"-1500.50": -150050,
		"  250  ":  25000,
	}
	for in, want := range cases {
		got, err := FromNairaString(in)
		require.NoError(t, err, "input %q", in)
		assert.Equal(t, want, got, "input %q", in)
	}
}

func TestFromNairaChecked(t *testing.T) {
	got, err := FromNairaChecked(1500)
	require.NoError(t, err)
	assert.Equal(t, Kobo(150000), got)

	_, err = FromNairaChecked(math.MaxInt64/KoboPerNaira + 1)
	require.ErrorIs(t, err, ErrOverflow)

	_, err = FromNairaChecked(math.MinInt64/KoboPerNaira - 1)
	require.ErrorIs(t, err, ErrOverflow)
}

// The sign-comparison form of the overflow check missed this case: two MinInt64
// operands wrap to exactly zero, so "result > 0" never fires and the caller gets
// a silently wrong total.
func TestAdd_MinInt64PairIsDetected(t *testing.T) {
	_, err := Kobo(math.MinInt64).Add(Kobo(math.MinInt64))
	require.ErrorIs(t, err, ErrOverflow)
}

func TestAdd_OverflowBoundaries(t *testing.T) {
	_, err := Kobo(math.MaxInt64).Add(1)
	require.ErrorIs(t, err, ErrOverflow)

	_, err = Kobo(math.MinInt64).Add(-1)
	require.ErrorIs(t, err, ErrOverflow)

	got, err := Kobo(math.MaxInt64 - 1).Add(1)
	require.NoError(t, err)
	assert.Equal(t, Kobo(math.MaxInt64), got)
}

func TestAdd_OrdinaryArithmeticUnaffected(t *testing.T) {
	got, err := Kobo(500).Add(-300)
	require.NoError(t, err)
	assert.Equal(t, Kobo(200), got)

	got, err = Kobo(-500).Add(300)
	require.NoError(t, err)
	assert.Equal(t, Kobo(-200), got)
}

// Sub used to be Add(-other); negating MinInt64 wraps back to MinInt64, which
// silently turned the subtraction into an addition.
func TestSub_MinInt64OperandIsDetected(t *testing.T) {
	_, err := Kobo(0).Sub(Kobo(math.MinInt64))
	require.ErrorIs(t, err, ErrOverflow)
}

func TestSub_OverflowBoundaries(t *testing.T) {
	_, err := Kobo(math.MinInt64).Sub(1)
	require.ErrorIs(t, err, ErrOverflow)

	_, err = Kobo(math.MaxInt64).Sub(-1)
	require.ErrorIs(t, err, ErrOverflow)

	got, err := Kobo(1000).Sub(400)
	require.NoError(t, err)
	assert.Equal(t, Kobo(600), got)
}

// Sum is the fold used for payroll batch totals — it must inherit Add's checks.
func TestSum_PropagatesOverflow(t *testing.T) {
	_, err := Sum([]Kobo{math.MaxInt64, 1})
	require.ErrorIs(t, err, ErrOverflow)

	got, err := Sum([]Kobo{100, 200, 300})
	require.NoError(t, err)
	assert.Equal(t, Kobo(600), got)
}

// Monnify sends decimal Naira on the wire. Parsing "1500.50" through the string
// path must land on exactly 150050 kobo with no float64 in between.
func TestFromNairaString_NoFloatPrecisionLoss(t *testing.T) {
	// Values chosen because they are not exactly representable in binary
	// floating point: routing them through float64 would land a kobo off.
	cases := map[string]Kobo{
		"0.01":       1,
		"0.10":       10,
		"10.05":      1005,
		"99999.99":   9999999,
		"1234567.89": 123456789,
	}
	for in, want := range cases {
		got, err := FromNairaString(in)
		require.NoError(t, err, "input %q", in)
		assert.Equal(t, want, got, "input %q must parse exactly", in)
	}
}

// The float boundary helper is lossy by nature, but it must round to the
// nearest kobo rather than truncating toward zero. Values here sit clear of the
// half-kobo boundary so the assertion tests the rounding rule and not the
// binary representation of the literal.
func TestFromNairaFloat_RoundsToNearestKobo(t *testing.T) {
	assert.Equal(t, Kobo(1008), FromNairaFloat(10.0784)) // .84 of a kobo rounds up
	assert.Equal(t, Kobo(1007), FromNairaFloat(10.0712)) // .12 of a kobo rounds down
	assert.Equal(t, Kobo(-1008), FromNairaFloat(-10.0784))
	assert.Equal(t, Kobo(-1007), FromNairaFloat(-10.0712))

	assert.Equal(t, Kobo(0), FromNairaFloat(math.NaN()))
	assert.Equal(t, Kobo(0), FromNairaFloat(math.Inf(1)))
	assert.Equal(t, Kobo(0), FromNairaFloat(math.Inf(-1)))
}
