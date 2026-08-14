package money

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// Currency is an ISO 4217 alphabetic code.
//
// Money in this system is always an integer count of a currency's *minor unit*
// (kobo, cent, pesewa). The exponent differs per currency — NGN and USD use 2,
// JPY uses 0, KWD uses 3 — so an amount is meaningless without knowing which
// currency it belongs to. That is the entire reason this type exists: an int64
// on its own cannot be compared, summed, or balanced safely.
type Currency string

// Currencies the platform can hold or move. Deliberately limited to what the
// configured PSPs actually settle; adding one here without a provider that can
// pay it out produces balances nobody can withdraw.
const (
	NGN Currency = "NGN" // Nigerian Naira
	USD Currency = "USD" // US Dollar
	GHS Currency = "GHS" // Ghanaian Cedi
	KES Currency = "KES" // Kenyan Shilling
	ZAR Currency = "ZAR" // South African Rand
	XOF Currency = "XOF" // West African CFA franc
	GBP Currency = "GBP" // Pound Sterling
	EUR Currency = "EUR" // Euro
)

// minorUnitExponent is the power of ten between the major and minor unit.
// XOF has no minor unit at all, which is exactly the sort of detail that
// produces 100× errors when code assumes "cents".
var minorUnitExponent = map[Currency]int{
	NGN: 2,
	USD: 2,
	GHS: 2,
	KES: 2,
	ZAR: 2,
	GBP: 2,
	EUR: 2,
	XOF: 0,
}

// symbols are for display only. Never parse from these.
var symbols = map[Currency]string{
	NGN: "₦", USD: "$", GHS: "₵", KES: "KSh",
	ZAR: "R", GBP: "£", EUR: "€", XOF: "CFA",
}

var (
	// ErrUnknownCurrency is returned for a code outside the supported set.
	ErrUnknownCurrency = errors.New("money: unsupported currency")
	// ErrCurrencyMismatch is returned when an operation spans two currencies.
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
)

// IsValid reports whether the currency is one the platform supports.
func (c Currency) IsValid() bool {
	_, ok := minorUnitExponent[c]
	return ok
}

// Exponent returns the number of decimal places in the currency's minor unit.
func (c Currency) Exponent() (int, error) {
	e, ok := minorUnitExponent[c]
	if !ok {
		return 0, fmt.Errorf("%w: %q", ErrUnknownCurrency, c)
	}
	return e, nil
}

// Scale returns the number of minor units in one major unit (100 for NGN, 1 for XOF).
func (c Currency) Scale() (int64, error) {
	e, err := c.Exponent()
	if err != nil {
		return 0, err
	}
	scale := int64(1)
	for i := 0; i < e; i++ {
		scale *= 10
	}
	return scale, nil
}

// Symbol returns the display symbol, falling back to the code itself.
func (c Currency) Symbol() string {
	if s, ok := symbols[c]; ok {
		return s
	}
	return string(c)
}

func (c Currency) String() string { return string(c) }

// ParseCurrency validates and normalises a currency code.
func ParseCurrency(s string) (Currency, error) {
	c := Currency(strings.ToUpper(strings.TrimSpace(s)))
	if !c.IsValid() {
		return "", fmt.Errorf("%w: %q", ErrUnknownCurrency, s)
	}
	return c, nil
}

// Value implements driver.Valuer so Currency persists as TEXT.
func (c Currency) Value() (driver.Value, error) {
	if !c.IsValid() {
		return nil, fmt.Errorf("%w: %q", ErrUnknownCurrency, c)
	}
	return string(c), nil
}

// Scan implements sql.Scanner.
func (c *Currency) Scan(value interface{}) error {
	switch v := value.(type) {
	case nil:
		return errors.New("money: currency cannot be NULL")
	case string:
		parsed, err := ParseCurrency(v)
		if err != nil {
			return err
		}
		*c = parsed
		return nil
	case []byte:
		parsed, err := ParseCurrency(string(v))
		if err != nil {
			return err
		}
		*c = parsed
		return nil
	default:
		return fmt.Errorf("money: cannot scan %T as currency", value)
	}
}

// Money is an amount in a specific currency, held as an integer count of that
// currency's minor unit.
//
// Every arithmetic method refuses to operate across currencies. There is no
// implicit conversion anywhere in this type: converting money requires an FX
// rate, an FX rate has a timestamp and a spread, and burying that inside an
// arithmetic operator is how systems silently lose money.
type Money struct {
	Minor    int64    `json:"minor"`
	Currency Currency `json:"currency"`
}

// New builds a Money from minor units, validating the currency.
func New(minor int64, c Currency) (Money, error) {
	if !c.IsValid() {
		return Money{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, c)
	}
	return Money{Minor: minor, Currency: c}, nil
}

// FromMajor builds a Money from a whole-unit amount (e.g. 1500 → ₦1,500.00),
// with overflow detection.
func FromMajor(major int64, c Currency) (Money, error) {
	scale, err := c.Scale()
	if err != nil {
		return Money{}, err
	}
	if scale != 0 && (major > maxInt64/scale || major < minInt64/scale) {
		return Money{}, ErrOverflow
	}
	return Money{Minor: major * scale, Currency: c}, nil
}

const (
	maxInt64 = int64(^uint64(0) >> 1)
	minInt64 = -maxInt64 - 1
)

// NGNFromKobo bridges the existing NGN-only Kobo type into Money. The payroll
// path still speaks Kobo; the ledger speaks Money.
func NGNFromKobo(k Kobo) Money {
	return Money{Minor: int64(k), Currency: NGN}
}

// Kobo narrows a Money back to the NGN-only type, refusing other currencies.
func (m Money) Kobo() (Kobo, error) {
	if m.Currency != NGN {
		return 0, fmt.Errorf("%w: cannot express %s as kobo", ErrCurrencyMismatch, m.Currency)
	}
	return Kobo(m.Minor), nil
}

// assertSame guards every cross-value operation.
func (m Money) assertSame(other Money) error {
	if m.Currency != other.Currency {
		return fmt.Errorf("%w: %s vs %s", ErrCurrencyMismatch, m.Currency, other.Currency)
	}
	return nil
}

// Add returns m + other, refusing mixed currencies and detecting overflow.
func (m Money) Add(other Money) (Money, error) {
	if err := m.assertSame(other); err != nil {
		return Money{}, err
	}
	result := m.Minor + other.Minor
	if (other.Minor > 0 && result < m.Minor) || (other.Minor < 0 && result > m.Minor) {
		return Money{}, ErrOverflow
	}
	return Money{Minor: result, Currency: m.Currency}, nil
}

// Sub returns m - other, refusing mixed currencies and detecting overflow.
func (m Money) Sub(other Money) (Money, error) {
	if err := m.assertSame(other); err != nil {
		return Money{}, err
	}
	result := m.Minor - other.Minor
	if (other.Minor < 0 && result < m.Minor) || (other.Minor > 0 && result > m.Minor) {
		return Money{}, ErrOverflow
	}
	return Money{Minor: result, Currency: m.Currency}, nil
}

// Percent returns m × num/denom with banker's rounding, staying in-currency.
func (m Money) Percent(numerator, denominator int64) (Money, error) {
	k, err := Kobo(m.Minor).Percent(numerator, denominator)
	if err != nil {
		return Money{}, err
	}
	return Money{Minor: int64(k), Currency: m.Currency}, nil
}

// Compare returns -1, 0, or 1. Errors on mixed currencies rather than lying.
func (m Money) Compare(other Money) (int, error) {
	if err := m.assertSame(other); err != nil {
		return 0, err
	}
	switch {
	case m.Minor < other.Minor:
		return -1, nil
	case m.Minor > other.Minor:
		return 1, nil
	default:
		return 0, nil
	}
}

// IsZero reports whether the amount is exactly zero.
func (m Money) IsZero() bool { return m.Minor == 0 }

// IsPositive reports whether the amount is strictly greater than zero.
func (m Money) IsPositive() bool { return m.Minor > 0 }

// IsNegative reports whether the amount is strictly less than zero.
func (m Money) IsNegative() bool { return m.Minor < 0 }

// String renders the amount with the right number of decimal places for its
// currency — "₦1,500.50", "CFA 1,500" (XOF has no minor unit).
func (m Money) String() string {
	exp, err := m.Currency.Exponent()
	if err != nil {
		return fmt.Sprintf("%d %s", m.Minor, m.Currency)
	}
	scale, _ := m.Currency.Scale()

	negative := m.Minor < 0
	abs := m.Minor
	if negative {
		abs = -abs
	}
	sign := ""
	if negative {
		sign = "-"
	}

	if exp == 0 {
		return fmt.Sprintf("%s%s%s", sign, m.Currency.Symbol(), withThousandsSep(abs))
	}
	whole := abs / scale
	frac := abs % scale
	return fmt.Sprintf("%s%s%s.%0*d", sign, m.Currency.Symbol(), withThousandsSep(whole), exp, frac)
}

// SumMoney folds a slice into one total, refusing mixed currencies. An empty
// slice has no currency, so the caller must supply the expected one.
func SumMoney(values []Money, expected Currency) (Money, error) {
	if !expected.IsValid() {
		return Money{}, fmt.Errorf("%w: %q", ErrUnknownCurrency, expected)
	}
	total := Money{Minor: 0, Currency: expected}
	for _, v := range values {
		next, err := total.Add(v)
		if err != nil {
			return Money{}, err
		}
		total = next
	}
	return total, nil
}

// ParseMoney parses a decimal major-unit string in a given currency, exactly —
// no float64 anywhere in the path.
func ParseMoney(s string, c Currency) (Money, error) {
	exp, err := c.Exponent()
	if err != nil {
		return Money{}, err
	}

	s = strings.TrimSpace(s)
	if s == "" {
		return Money{}, ErrInvalidFormat
	}
	negative := strings.HasPrefix(s, "-")
	if negative {
		s = s[1:]
	}

	parts := strings.SplitN(s, ".", 2)
	whole, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return Money{}, ErrInvalidFormat
	}

	var fraction int64
	if len(parts) == 2 {
		frac := parts[1]
		if exp == 0 {
			return Money{}, fmt.Errorf("%w: %s has no minor unit", ErrInvalidFormat, c)
		}
		if len(frac) == 0 || len(frac) > exp {
			return Money{}, ErrInvalidFormat
		}
		for len(frac) < exp {
			frac += "0"
		}
		fraction, err = strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return Money{}, ErrInvalidFormat
		}
	}

	scale, _ := c.Scale()
	if scale != 0 && whole > (maxInt64-fraction)/scale {
		return Money{}, ErrOverflow
	}
	total := whole*scale + fraction
	if negative {
		total = -total
	}
	return Money{Minor: total, Currency: c}, nil
}
