package validate

import (
	"strings"
	"testing"

	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
)

func TestPhone(t *testing.T) {
	for _, p := range []string{"+2348012345678", "+14155550123"} {
		assert.True(t, Phone(p), p)
	}
	for _, p := range []string{"", "08012345678", "2348012345678", "+0348012345678", "+234 801 234 5678", "+234801234567890123", "+234abc45678"} {
		assert.False(t, Phone(p), p)
	}
}

func TestBankDetails(t *testing.T) {
	assert.Empty(t, BankDetails(money.NGN, "0123456789", "058"))
	assert.Empty(t, BankDetails(money.NGN, "0123456789", "100004"))
	assert.NotEmpty(t, BankDetails(money.NGN, "012345678", "058"), "9 digits is not a NUBAN")
	assert.NotEmpty(t, BankDetails(money.NGN, "01234567890", "058"))
	assert.NotEmpty(t, BankDetails(money.NGN, "01234x6789", "058"))
	assert.NotEmpty(t, BankDetails(money.NGN, "0123456789", "GTB"))
	assert.Empty(t, BankDetails(money.KES, "ABCD123456", "01"))
	assert.NotEmpty(t, BankDetails(money.KES, "12", "01"))
}

func TestBVN(t *testing.T) {
	assert.True(t, BVN("12345678901"))
	assert.False(t, BVN("1234567890"))
	assert.False(t, BVN("1234567890a"))
}

func TestEmail(t *testing.T) {
	for _, e := range []string{"a@b.ng", "first.last+tag@sub.example.com"} {
		assert.True(t, Email(e), e)
	}
	for _, e := range []string{"", "nope", "a@", "@b.ng", "A <a@b.ng>", "a@b.ng, c@d.ng", " a@b.ng"} {
		assert.False(t, Email(e), "%q", e)
	}
}

func TestPassword(t *testing.T) {
	assert.NotEmpty(t, Password("short"))
	assert.NotEmpty(t, Password(strings.Repeat("a", 73)))
	assert.Empty(t, Password(strings.Repeat("a", 12)))
	assert.Empty(t, Password(strings.Repeat("a", 72)))
}
