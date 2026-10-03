// Package validate holds the field-format rules shared by the API handlers and
// the services behind them (a service enforcing an invariant can't import the
// handler layer). Pure functions, no I/O.
package validate

import (
	"net/mail"
	"regexp"

	"go-payroll-engine/pkg/money"
)

var (
	// E.164: a leading +, a country code that doesn't start with 0, and at
	// most 15 digits in all. One canonical form matters because users.phone
	// is UNIQUE and is the login identity — "0801..." and "+234801..." must
	// not become two different accounts for the same SIM.
	e164Pattern = regexp.MustCompile(`^\+[1-9][0-9]{7,14}$`)
	// NUBAN — the CBN's 10-digit Nigerian account number.
	nubanPattern = regexp.MustCompile(`^[0-9]{10}$`)
	// BVN — 11 digits.
	bvnPattern = regexp.MustCompile(`^[0-9]{11}$`)
	// Nigerian bank/institution codes are 3 (CBN) to 6 (NIP) digits.
	ngBankCodePattern = regexp.MustCompile(`^[0-9]{3,6}$`)
	// Outside NGN there's no single scheme; bound it to something sane.
	genericAccountPattern = regexp.MustCompile(`^[0-9A-Za-z]{4,34}$`)
)

// Phone reports whether p is a valid E.164 number.
func Phone(p string) bool { return e164Pattern.MatchString(p) }

// BVN reports whether b is a valid 11-digit BVN.
func BVN(b string) bool { return bvnPattern.MatchString(b) }

// BankDetails returns a client-facing message when an account number or
// bank code can't possibly be valid for currency, or "" when they can. A bad
// account number otherwise surfaces only when the bank rejects the transfer
// on payday, as a failed salary payment.
func BankDetails(currency money.Currency, accountNumber, bankCode string) string {
	if currency == money.NGN {
		if !nubanPattern.MatchString(accountNumber) {
			return "account_number must be a 10-digit NUBAN"
		}
		if !ngBankCodePattern.MatchString(bankCode) {
			return "bank_code must be 3 to 6 digits"
		}
		return ""
	}
	if !genericAccountPattern.MatchString(accountNumber) {
		return "account_number is not valid"
	}
	if bankCode == "" {
		return "bank_code is required"
	}
	return ""
}

// Email reports whether e is a plausible bare address ("a@b.ng"). It rejects
// display-name forms ("A <a@b.ng>"), which net/mail would otherwise accept.
func Email(e string) bool {
	addr, err := mail.ParseAddress(e)
	return err == nil && addr.Address == e
}
