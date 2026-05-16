package money

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFromNaira(t *testing.T) {
	assert.Equal(t, Kobo(150000), FromNaira(1500))
	assert.Equal(t, Kobo(0), FromNaira(0))
	assert.Equal(t, Kobo(-100), FromNaira(-1))
}

func TestFromNairaString(t *testing.T) {
	cases := []struct {
		in   string
		want Kobo
		err  bool
	}{
		{"1500.50", 150050, false},
		{"1500", 150000, false},
		{"0.99", 99, false},
		{"0.01", 1, false},
		{"0", 0, false},
		{"1500.5", 150050, false},   // single-digit fraction is right-padded
		{"-1500.50", -150050, false}, // negative supported
		{"  1500.50  ", 150050, false}, // whitespace trimmed
		{"1500.555", 0, true},  // > 2 decimals is precision loss → reject
		{"1500.", 0, true},     // trailing dot
		{"abc", 0, true},
		{"", 0, true},
		{"1500.ab", 0, true},
	}
	for _, c := range cases {
		got, err := FromNairaString(c.in)
		if c.err {
			assert.Error(t, err, "input %q should error", c.in)
		} else {
			require.NoError(t, err, "input %q should not error", c.in)
			assert.Equal(t, c.want, got, "input %q", c.in)
		}
	}
}

func TestString(t *testing.T) {
	assert.Equal(t, "₦1,500.50", FromNaira(1500).MustAdd(50).String())
	assert.Equal(t, "₦0.00", Kobo(0).String())
	assert.Equal(t, "₦0.99", Kobo(99).String())
	assert.Equal(t, "-₦100.00", FromNaira(-100).String())
	assert.Equal(t, "₦1,000,000.00", FromNaira(1_000_000).String())
	assert.Equal(t, "₦12,345,678.90", Kobo(1_234_567_890).String())
}

func TestAdd_NoOverflow(t *testing.T) {
	a := Kobo(100)
	b := Kobo(200)
	sum, err := a.Add(b)
	require.NoError(t, err)
	assert.Equal(t, Kobo(300), sum)
}

func TestAdd_OverflowDetected(t *testing.T) {
	max := Kobo(math.MaxInt64)
	_, err := max.Add(1)
	assert.ErrorIs(t, err, ErrOverflow)

	min := Kobo(math.MinInt64)
	_, err = min.Add(-1)
	assert.ErrorIs(t, err, ErrOverflow)
}

func TestSub(t *testing.T) {
	r, err := Kobo(300).Sub(Kobo(100))
	require.NoError(t, err)
	assert.Equal(t, Kobo(200), r)

	r, err = Kobo(100).Sub(Kobo(300))
	require.NoError(t, err)
	assert.Equal(t, Kobo(-200), r)
}

func TestMulInt(t *testing.T) {
	r, err := FromNaira(5000).MulInt(18) // 5000 NGN × 18 days
	require.NoError(t, err)
	assert.Equal(t, FromNaira(90000), r)

	// Zero short-circuits cleanly.
	r, err = Kobo(0).MulInt(1000)
	require.NoError(t, err)
	assert.Equal(t, Kobo(0), r)

	// Overflow detection.
	_, err = Kobo(math.MaxInt64 / 2).MulInt(3)
	assert.ErrorIs(t, err, ErrOverflow)
}

func TestPercent_BasicCases(t *testing.T) {
	// 40% of ₦90,000 = ₦36,000 — the canonical EWA cap calculation.
	earned := FromNaira(90000)
	cap, err := earned.Percent(40, 100)
	require.NoError(t, err)
	assert.Equal(t, FromNaira(36000), cap)

	// 0%
	r, err := FromNaira(100).Percent(0, 100)
	require.NoError(t, err)
	assert.Equal(t, Kobo(0), r)

	// 100%
	r, err = FromNaira(100).Percent(100, 100)
	require.NoError(t, err)
	assert.Equal(t, FromNaira(100), r)
}

func TestPercent_BankersRounding(t *testing.T) {
	// 50% of 5 kobo = 2.5 kobo → rounds to even (2).
	r, err := Kobo(5).Percent(50, 100)
	require.NoError(t, err)
	assert.Equal(t, Kobo(2), r, "5 × 50/100 should round half-to-even → 2")

	// 50% of 7 kobo = 3.5 kobo → rounds to even (4).
	r, err = Kobo(7).Percent(50, 100)
	require.NoError(t, err)
	assert.Equal(t, Kobo(4), r, "7 × 50/100 should round half-to-even → 4")

	// Division by zero rejected.
	_, err = Kobo(100).Percent(50, 0)
	assert.Error(t, err)
}

func TestPredicates(t *testing.T) {
	assert.True(t, Kobo(0).IsZero())
	assert.True(t, Kobo(1).IsPositive())
	assert.True(t, Kobo(-1).IsNegative())
	assert.False(t, Kobo(0).IsPositive())
	assert.False(t, Kobo(0).IsNegative())
}

func TestSum(t *testing.T) {
	total, err := Sum([]Kobo{FromNaira(100), FromNaira(200), FromNaira(300)})
	require.NoError(t, err)
	assert.Equal(t, FromNaira(600), total)

	// Empty slice → zero.
	total, err = Sum(nil)
	require.NoError(t, err)
	assert.Equal(t, Zero, total)
}

func TestMarshalJSON(t *testing.T) {
	type wrapper struct {
		Amount Kobo `json:"amount"`
	}

	w := wrapper{Amount: FromNaira(1500)}
	out, err := json.Marshal(w)
	require.NoError(t, err)
	assert.JSONEq(t, `{"amount":150000}`, string(out))
}

func TestUnmarshalJSON_Integer(t *testing.T) {
	type wrapper struct {
		Amount Kobo `json:"amount"`
	}
	var w wrapper
	err := json.Unmarshal([]byte(`{"amount":150000}`), &w)
	require.NoError(t, err)
	assert.Equal(t, FromNaira(1500), w.Amount)
}

func TestUnmarshalJSON_String(t *testing.T) {
	type wrapper struct {
		Amount Kobo `json:"amount"`
	}
	var w wrapper
	err := json.Unmarshal([]byte(`{"amount":"1500.50"}`), &w)
	require.NoError(t, err)
	assert.Equal(t, Kobo(150050), w.Amount)
}

func TestUnmarshalJSON_FloatRejected(t *testing.T) {
	type wrapper struct {
		Amount Kobo `json:"amount"`
	}
	var w wrapper
	// Non-integer float is a precision hazard and must be rejected.
	err := json.Unmarshal([]byte(`{"amount":1500.50}`), &w)
	assert.Error(t, err, "non-integer floats must be rejected to prevent silent precision loss")
}

func TestUnmarshalJSON_IntegerFloatAllowed(t *testing.T) {
	type wrapper struct {
		Amount Kobo `json:"amount"`
	}
	var w wrapper
	// "150000.0" is integer-valued — allowed.
	err := json.Unmarshal([]byte(`{"amount":150000.0}`), &w)
	require.NoError(t, err)
	assert.Equal(t, FromNaira(1500), w.Amount)
}

func TestRoundTripJSON(t *testing.T) {
	original := FromNaira(123456)
	data, err := json.Marshal(original)
	require.NoError(t, err)

	var decoded Kobo
	err = json.Unmarshal(data, &decoded)
	require.NoError(t, err)
	assert.Equal(t, original, decoded)
}
