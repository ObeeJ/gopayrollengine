package handlers

import (
	"fmt"
	"math/rand"
	"testing"

	"go-payroll-engine/pkg/money"

	"github.com/stretchr/testify/assert"
)

func TestValidPhone(t *testing.T) {
	for _, p := range []string{"+2348012345678", "+14155550123"} {
		assert.True(t, validPhone(p), p)
	}
	for _, p := range []string{"", "08012345678", "2348012345678", "+0348012345678", "+234 801 234 5678", "+234801234567890123", "+234abc45678"} {
		assert.False(t, validPhone(p), p)
	}
}

func TestBankDetailsError(t *testing.T) {
	assert.Empty(t, bankDetailsError(money.NGN, "0123456789", "058"))
	assert.Empty(t, bankDetailsError(money.NGN, "0123456789", "100004"))
	assert.NotEmpty(t, bankDetailsError(money.NGN, "012345678", "058"), "9 digits is not a NUBAN")
	assert.NotEmpty(t, bankDetailsError(money.NGN, "01234567890", "058"))
	assert.NotEmpty(t, bankDetailsError(money.NGN, "01234x6789", "058"))
	assert.NotEmpty(t, bankDetailsError(money.NGN, "0123456789", "GTB"))
	assert.Empty(t, bankDetailsError(money.KES, "ABCD123456", "01"))
	assert.NotEmpty(t, bankDetailsError(money.KES, "12", "01"))
}

func TestValidBVN(t *testing.T) {
	assert.True(t, validBVN("12345678901"))
	assert.False(t, validBVN("1234567890"))
	assert.False(t, validBVN("1234567890a"))
}

// testPhone — a fresh, valid E.164 Nigerian mobile number per call, so
// integration tests don't collide on users.phone's UNIQUE constraint.
func testPhone() string {
	return fmt.Sprintf("+23480%08d", rand.Intn(100_000_000))
}
