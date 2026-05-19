// Package money — integer Kobo arithmetic; floats are not invited.
package money

import (
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Kobo — int64 minor units of NGN; the only money type the system trusts.
type Kobo int64

// KoboPerNaira is the conversion factor from major to minor units.
const KoboPerNaira = 100

// Zero is a convenience constant for amounts of zero kobo.
const Zero Kobo = 0

// ErrNegativeAmount is returned by constructors that disallow negative values.
var ErrNegativeAmount = errors.New("money: amount must be non-negative")

// ErrInvalidFormat is returned when a string cannot be parsed as a Naira amount.
var ErrInvalidFormat = errors.New("money: invalid naira string format")

// ErrOverflow is returned when an arithmetic operation would exceed int64 range.
var ErrOverflow = errors.New("money: arithmetic overflow")

// FromNaira returns Kobo for a whole-Naira amount.
func FromNaira(naira int64) Kobo {
	return Kobo(naira * KoboPerNaira)
}

// FromNairaString parses a decimal Naira string into Kobo — at most two fractional digits.
func FromNairaString(s string) (Kobo, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, ErrInvalidFormat
	}

	negative := false
	if strings.HasPrefix(s, "-") {
		negative = true
		s = s[1:]
	}

	parts := strings.SplitN(s, ".", 2)
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, ErrInvalidFormat
	}

	var fraction int64
	if len(parts) == 2 {
		frac := parts[1]
		if len(frac) == 0 || len(frac) > 2 {
			return 0, ErrInvalidFormat
		}
		// Right-pad single-digit fractions: "1500.5" → "50" kobo.
		if len(frac) == 1 {
			frac += "0"
		}
		fraction, err = strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, ErrInvalidFormat
		}
	}

	total := whole*KoboPerNaira + fraction
	if negative {
		total = -total
	}
	return Kobo(total), nil
}

// Naira returns a lossy float64 view — display or external-API boundaries only.
func (k Kobo) Naira() float64 {
	return float64(k) / float64(KoboPerNaira)
}

// FromNairaFloat — boundary helper for float-typed upstreams; rounds half-away-from-zero.
func FromNairaFloat(f float64) Kobo {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return 0
	}
	scaled := f * KoboPerNaira
	if scaled >= 0 {
		return Kobo(int64(scaled + 0.5))
	}
	return Kobo(int64(scaled - 0.5))
}

// String returns a human-readable Naira string like "₦1,500.50".
func (k Kobo) String() string {
	negative := k < 0
	abs := k
	if negative {
		abs = -k
	}
	whole := int64(abs) / KoboPerNaira
	frac := int64(abs) % KoboPerNaira
	wholeStr := withThousandsSep(whole)
	sign := ""
	if negative {
		sign = "-"
	}
	return fmt.Sprintf("%s₦%s.%02d", sign, wholeStr, frac)
}

// withThousandsSep inserts commas every 3 digits from the right.
func withThousandsSep(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	// Walk from the right inserting commas every 3 digits.
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		if len(s) > pre {
			b.WriteByte(',')
		}
	}
	for i := pre; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}

// Add returns k + other with overflow detection.
func (k Kobo) Add(other Kobo) (Kobo, error) {
	result := k + other
	// Overflow detection for signed addition: sign of result differs from both operands.
	if (k > 0 && other > 0 && result < 0) || (k < 0 && other < 0 && result > 0) {
		return 0, ErrOverflow
	}
	return result, nil
}

// MustAdd is Add but panics on overflow. Use only for values known to be bounded.
func (k Kobo) MustAdd(other Kobo) Kobo {
	r, err := k.Add(other)
	if err != nil {
		panic(err)
	}
	return r
}

// Sub returns k - other with overflow detection.
func (k Kobo) Sub(other Kobo) (Kobo, error) {
	return k.Add(-other)
}

// MulInt multiplies by an integer scalar with overflow detection.
// Used for things like "salary × days worked".
func (k Kobo) MulInt(n int64) (Kobo, error) {
	if k == 0 || n == 0 {
		return 0, nil
	}
	result := int64(k) * n
	if result/n != int64(k) {
		return 0, ErrOverflow
	}
	return Kobo(result), nil
}

// Percent returns k × num/denom with banker's rounding to keep long runs unbiased.
func (k Kobo) Percent(numerator, denominator int64) (Kobo, error) {
	if denominator == 0 {
		return 0, errors.New("money: division by zero in Percent")
	}
	scaled, err := k.MulInt(numerator)
	if err != nil {
		return 0, err
	}
	// Integer division truncates; we want banker's rounding for the remainder.
	q := int64(scaled) / denominator
	r := int64(scaled) % denominator
	twiceR := 2 * r
	switch {
	case twiceR > denominator || twiceR < -denominator:
		if r > 0 {
			q++
		} else {
			q--
		}
	case twiceR == denominator || twiceR == -denominator:
		// Round half to even.
		if q%2 != 0 {
			if r > 0 {
				q++
			} else {
				q--
			}
		}
	}
	return Kobo(q), nil
}

// IsZero reports whether the amount is exactly zero.
func (k Kobo) IsZero() bool { return k == 0 }

// IsPositive reports whether the amount is strictly greater than zero.
func (k Kobo) IsPositive() bool { return k > 0 }

// IsNegative reports whether the amount is strictly less than zero.
func (k Kobo) IsNegative() bool { return k < 0 }

// MarshalJSON emits the value as an integer number of kobo — clients format for display.
func (k Kobo) MarshalJSON() ([]byte, error) {
	return []byte(strconv.FormatInt(int64(k), 10)), nil
}

// UnmarshalJSON accepts integer kobo or a decimal Naira string — floats are refused.
func (k *Kobo) UnmarshalJSON(data []byte) error {
	if len(data) == 0 {
		return ErrInvalidFormat
	}
	// String form: "1500.50"
	if data[0] == '"' {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		parsed, err := FromNairaString(s)
		if err != nil {
			return err
		}
		*k = parsed
		return nil
	}
	// Numeric form: integer kobo. Reject floats — they're a precision hazard.
	if strings.ContainsAny(string(data), ".eE") {
		f, err := strconv.ParseFloat(string(data), 64)
		if err != nil || math.Floor(f) != f {
			return fmt.Errorf("money: refusing to parse non-integer numeric (%s) as kobo; send a string or an integer", string(data))
		}
		*k = Kobo(int64(f))
		return nil
	}
	n, err := strconv.ParseInt(string(data), 10, 64)
	if err != nil {
		return ErrInvalidFormat
	}
	*k = Kobo(n)
	return nil
}

// Sum reduces a slice of Kobo to a single total, with overflow detection.
func Sum(values []Kobo) (Kobo, error) {
	var total Kobo
	for _, v := range values {
		next, err := total.Add(v)
		if err != nil {
			return 0, err
		}
		total = next
	}
	return total, nil
}

// Value implements driver.Valuer — Kobo persists as BIGINT (see migration 000005).
func (k Kobo) Value() (driver.Value, error) {
	return int64(k), nil
}

// Scan implements sql.Scanner — accepts int64, []byte, or string because drivers disagree.
func (k *Kobo) Scan(value interface{}) error {
	if value == nil {
		*k = 0
		return nil
	}
	switch v := value.(type) {
	case int64:
		*k = Kobo(v)
		return nil
	case int:
		*k = Kobo(v)
		return nil
	case []byte:
		n, err := strconv.ParseInt(string(v), 10, 64)
		if err != nil {
			return fmt.Errorf("money: cannot scan %q as kobo: %w", v, err)
		}
		*k = Kobo(n)
		return nil
	case string:
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("money: cannot scan %q as kobo: %w", v, err)
		}
		*k = Kobo(n)
		return nil
	default:
		return fmt.Errorf("money: cannot scan %T as kobo", value)
	}
}
