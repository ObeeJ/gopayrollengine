package models

import (
	"errors"
	"fmt"
	"time"

	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Direction is the side of an entry. It carries the sign so amounts stay positive.
type Direction string

const (
	Debit  Direction = "debit"
	Credit Direction = "credit"
)

// AccountType — the chart of accounts. Kept deliberately small; every new type
// is a new way for money to be classified, and therefore misclassified.
type AccountType string

const (
	// AccountEmployerFunding — the employer's pool that EWA draws are funded from.
	// Credit-normal: a balance here means the employer has money committed.
	AccountEmployerFunding AccountType = "employer_funding"

	// AccountAdvanceReceivable — per-worker. Debit-normal: a positive balance is
	// money advanced but not yet recovered from payroll.
	AccountAdvanceReceivable AccountType = "advance_receivable"

	// AccountWagePayable — per-worker. Credit-normal: wages owed to the worker.
	AccountWagePayable AccountType = "wage_payable"

	// AccountCashSettlement — the bank/Monnify boundary. Credit-normal.
	AccountCashSettlement AccountType = "cash_settlement"

	// AccountFeeIncome — credit-normal. Zero in the fee-free model, present so a
	// fee-charging deployment does not have to reshape the ledger.
	AccountFeeIncome AccountType = "fee_income"

	// AccountWriteOffExpense — org-level. Debit-normal: recognizes an
	// outstanding advance as an unrecoverable loss when there is no more
	// payroll to settle it against (a terminated employee). See
	// WriteOffAdvance. Distinct from a cancellation: cash genuinely left the
	// platform here, so the loss is booked, not reversed as if it never
	// happened.
	AccountWriteOffExpense AccountType = "write_off_expense"
)

// normalBalances maps each account type to its normal side.
var normalBalances = map[AccountType]Direction{
	AccountEmployerFunding:   Credit,
	AccountAdvanceReceivable: Debit,
	AccountWagePayable:       Credit,
	AccountCashSettlement:    Credit,
	AccountFeeIncome:         Credit,
	AccountWriteOffExpense:   Debit,
}

var (
	// ErrUnbalanced is returned before any write when debits != credits.
	ErrUnbalanced = errors.New("ledger: transaction does not balance")
	// ErrNoEntries is returned for a transaction with fewer than two lines.
	ErrNoEntries = errors.New("ledger: a transaction needs at least two entries")
	// ErrNonPositiveAmount is returned when an entry amount is zero or negative.
	ErrNonPositiveAmount = errors.New("ledger: entry amounts must be strictly positive")
	// ErrUnknownAccountType is returned for an account type outside the chart.
	ErrUnknownAccountType = errors.New("ledger: unknown account type")
)

// LedgerAccount — a bucket of value in exactly one currency. Balances are
// never stored here; they are derived by summing entries.
type LedgerAccount struct {
	ID             string         `gorm:"primaryKey" json:"id"`
	OrganizationID string         `gorm:"index;not null" json:"organization_id"`
	EmployeeID     *string        `json:"employee_id,omitempty"`
	AccountType    AccountType    `gorm:"not null" json:"account_type"`
	NormalBalance  Direction      `gorm:"not null" json:"normal_balance"`
	Currency       money.Currency `gorm:"not null" json:"currency"`
	CreatedAt      time.Time      `json:"created_at"`
}

func (LedgerAccount) TableName() string { return "ledger_accounts" }

// BeforeCreate — LACC- prefix keeps ledger IDs distinguishable in logs.
func (a *LedgerAccount) BeforeCreate(tx *gorm.DB) error {
	if a.ID == "" {
		a.ID = "LACC-" + uuid.New().String()[:8]
	}
	return nil
}

// LedgerTransaction — the atomic grouping. Entries only exist inside one.
type LedgerTransaction struct {
	ID             string    `gorm:"primaryKey" json:"id"`
	OrganizationID string    `gorm:"index;not null" json:"organization_id"`
	Kind           string    `gorm:"not null" json:"kind"`
	Reference      string    `json:"reference"`
	IdempotencyKey string    `gorm:"not null" json:"idempotency_key"`
	CreatedAt      time.Time `json:"created_at"`
}

func (LedgerTransaction) TableName() string { return "ledger_transactions" }

// BeforeCreate — LTX- prefix.
func (t *LedgerTransaction) BeforeCreate(tx *gorm.DB) error {
	if t.ID == "" {
		t.ID = "LTX-" + uuid.New().String()[:8]
	}
	return nil
}

// LedgerEntry — one debit or credit line. Append-only; enforced by trigger.
// AmountMinor is a count of Currency's minor unit, never a bare integer: the
// column name predates multi-currency and is kept to avoid burying the currency
// invariant inside a rename.
type LedgerEntry struct {
	ID             int64          `gorm:"primaryKey;autoIncrement" json:"id"`
	TransactionID  string         `gorm:"index;not null" json:"transaction_id"`
	AccountID      string         `gorm:"index;not null" json:"account_id"`
	OrganizationID string         `gorm:"index;not null" json:"organization_id"`
	Direction      Direction      `gorm:"not null" json:"direction"`
	AmountMinor    int64          `gorm:"column:amount_kobo;type:bigint;not null" json:"amount_minor"`
	Currency       money.Currency `gorm:"not null" json:"currency"`
	CreatedAt      time.Time      `json:"created_at"`
}

func (LedgerEntry) TableName() string { return "ledger_entries" }

// Money reconstitutes the entry's amount as a currency-qualified value.
func (e LedgerEntry) Money() money.Money {
	return money.Money{Minor: e.AmountMinor, Currency: e.Currency}
}

// EntryInput — one line of a posting request. Amount carries its own currency,
// which is what lets the validator reject a mixed-currency posting before it
// ever reaches the database.
type EntryInput struct {
	AccountID string
	Direction Direction
	Amount    money.Money
}

// PostingRequest — a complete, balanced, single-currency movement of value.
type PostingRequest struct {
	OrgID string
	Kind  string
	// Reference is the domain object this posting explains (advance ID, payroll ID).
	Reference string
	// IdempotencyKey makes a retry a no-op. Required — a money-moving call
	// without one is a double-post waiting for a network blip.
	IdempotencyKey string
	Entries        []EntryInput
}

// ValidatePosting checks a posting request without touching the database: an
// idempotency key is present, there are at least two lines, every amount is
// strictly positive, and debits equal credits.
//
// The deferred constraint trigger in migration 000013 is the real guarantee —
// it catches writers that never come through this function. This exists so the
// common path fails early with an error a human can act on.
func ValidatePosting(req PostingRequest) error {
	if req.IdempotencyKey == "" {
		return errors.New("ledger: idempotency key is required")
	}
	if len(req.Entries) < 2 {
		return ErrNoEntries
	}

	// Every entry must share one currency. Summing across currencies is
	// meaningless, so this is checked before any arithmetic happens rather than
	// letting a mixed posting produce a plausible-looking total.
	currency := req.Entries[0].Amount.Currency
	if !currency.IsValid() {
		return fmt.Errorf("%w: %q", money.ErrUnknownCurrency, currency)
	}

	debits := money.Money{Currency: currency}
	credits := money.Money{Currency: currency}

	for _, e := range req.Entries {
		if e.Amount.Currency != currency {
			return fmt.Errorf(
				"%w: transaction mixes %s and %s; cross-currency movement must be posted as separate single-currency transactions",
				money.ErrCurrencyMismatch, currency, e.Amount.Currency)
		}
		if !e.Amount.IsPositive() {
			return fmt.Errorf("%w: got %s", ErrNonPositiveAmount, e.Amount)
		}
		var err error
		switch e.Direction {
		case Debit:
			debits, err = debits.Add(e.Amount)
		case Credit:
			credits, err = credits.Add(e.Amount)
		default:
			return fmt.Errorf("ledger: invalid direction %q", e.Direction)
		}
		if err != nil {
			return fmt.Errorf("ledger: total overflow: %w", err)
		}
	}
	if debits.Minor != credits.Minor {
		return fmt.Errorf("%w: debits=%s credits=%s", ErrUnbalanced, debits, credits)
	}
	return nil
}

// PostTransaction writes a balanced set of entries inside the caller's tx.
//
// It must be called inside WithOrgScope so RLS applies. Balance is validated
// here for a clear error message and again by the deferred constraint trigger at
// COMMIT — the trigger is the real guarantee, this check is the good error.
//
// Idempotency: a replay with the same key returns the existing transaction and
// writes nothing.
func PostTransaction(tx *gorm.DB, req PostingRequest) (*LedgerTransaction, error) {
	if err := ValidatePosting(req); err != nil {
		return nil, err
	}

	// Replay check before insert — cheaper than relying on the unique violation,
	// and lets us hand back the original transaction.
	var existing LedgerTransaction
	err := tx.Where("organization_id = ? AND idempotency_key = ?", req.OrgID, req.IdempotencyKey).
		First(&existing).Error
	if err == nil {
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	ltx := LedgerTransaction{
		OrganizationID: req.OrgID,
		Kind:           req.Kind,
		Reference:      req.Reference,
		IdempotencyKey: req.IdempotencyKey,
	}
	if err := tx.Create(&ltx).Error; err != nil {
		return nil, err
	}

	entries := make([]LedgerEntry, 0, len(req.Entries))
	for _, e := range req.Entries {
		entries = append(entries, LedgerEntry{
			TransactionID:  ltx.ID,
			AccountID:      e.AccountID,
			OrganizationID: req.OrgID,
			Direction:      e.Direction,
			AmountMinor:    e.Amount.Minor,
			Currency:       e.Amount.Currency,
		})
	}
	if err := tx.Create(&entries).Error; err != nil {
		return nil, err
	}

	return &ltx, nil
}

// EnsureAccount returns the account for (org, employee, type, currency),
// creating it on first use. employeeID may be empty for org-level accounts.
//
// Currency is part of the account's identity, not a property of it: a worker
// holding both NGN and USD has two receivable accounts, because one account
// cannot have a meaningful balance in two currencies.
func EnsureAccount(
	tx *gorm.DB, orgID, employeeID string, at AccountType, currency money.Currency,
) (*LedgerAccount, error) {
	normal, ok := normalBalances[at]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownAccountType, at)
	}
	if !currency.IsValid() {
		return nil, fmt.Errorf("%w: %q", money.ErrUnknownCurrency, currency)
	}

	q := tx.Where("organization_id = ? AND account_type = ? AND currency = ?", orgID, at, currency)
	if employeeID == "" {
		q = q.Where("employee_id IS NULL")
	} else {
		q = q.Where("employee_id = ?", employeeID)
	}

	var acct LedgerAccount
	err := q.First(&acct).Error
	if err == nil {
		return &acct, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	acct = LedgerAccount{
		OrganizationID: orgID,
		AccountType:    at,
		NormalBalance:  normal,
		Currency:       currency,
	}
	if employeeID != "" {
		acct.EmployeeID = &employeeID
	}
	if err := tx.Create(&acct).Error; err != nil {
		return nil, err
	}
	return &acct, nil
}

// AccountBalance derives the balance by summing entries — never by reading a
// stored total. Returned in the account's normal direction and its own
// currency, so a debit-normal account with more debits than credits reports a
// positive balance.
func AccountBalance(tx *gorm.DB, accountID string) (money.Money, error) {
	var acct LedgerAccount
	if err := tx.First(&acct, "id = ?", accountID).Error; err != nil {
		return money.Money{}, err
	}

	var row struct {
		Debits  int64
		Credits int64
	}
	if err := tx.Model(&LedgerEntry{}).
		Select(`COALESCE(SUM(amount_kobo) FILTER (WHERE direction = 'debit'), 0)  AS debits,
		        COALESCE(SUM(amount_kobo) FILTER (WHERE direction = 'credit'), 0) AS credits`).
		Where("account_id = ?", accountID).
		Scan(&row).Error; err != nil {
		return money.Money{}, err
	}

	balance := row.Credits - row.Debits
	if acct.NormalBalance == Debit {
		balance = row.Debits - row.Credits
	}
	return money.Money{Minor: balance, Currency: acct.Currency}, nil
}
