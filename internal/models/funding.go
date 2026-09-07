package models

import (
	"time"

	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
)

// OrganizationFundingAccount is the dedicated bank account number an
// employer deposits into to fund its EWA pool — one per org. It carries no
// balance of its own; the balance lives entirely in the ledger (see
// RecordEmployerFunding). This row exists only to remember which provider
// account belongs to which org, for display and for the webhook lookup.
type OrganizationFundingAccount struct {
	OrganizationID string `gorm:"primaryKey" json:"organization_id"`
	ProviderName   string `gorm:"column:provider_name;not null" json:"provider_name"`

	AccountReference string `gorm:"column:account_reference;not null" json:"account_reference"`
	AccountNumber    string `gorm:"column:account_number;not null" json:"account_number"`
	AccountName      string `gorm:"column:account_name;not null" json:"account_name"`
	BankName         string `gorm:"column:bank_name;not null" json:"bank_name"`
	BankCode         string `gorm:"column:bank_code" json:"bank_code,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (OrganizationFundingAccount) TableName() string { return "organization_funding_accounts" }

// RecordEmployerFunding posts a deposit into the org's EWA funding pool:
//
//	Dr cash_settlement (org)   offsets prior advances against this deposit
//	Cr employer_funding (org)  the pool's own balance grows
//
// postAdvanceLedger's existing entries (Dr advance_receivable / Cr
// cash_settlement) are unchanged by this — see migration 000019's comment for
// why. The consequence: cash_settlement's own balance becomes exactly
// "cumulative advances disbursed minus cumulative deposits", which is what
// FundingExposure reads.
//
// Idempotent on (orgID, providerReference): a duplicate webhook delivery for
// the same deposit returns the existing transaction and posts nothing twice.
func RecordEmployerFunding(tx *gorm.DB, orgID string, amount money.Money, providerReference string) error {
	cash, err := EnsureAccount(tx, orgID, "", AccountCashSettlement, amount.Currency)
	if err != nil {
		return err
	}
	funding, err := EnsureAccount(tx, orgID, "", AccountEmployerFunding, amount.Currency)
	if err != nil {
		return err
	}

	_, err = PostTransaction(tx, PostingRequest{
		OrgID:          orgID,
		Kind:           "employer_funding_deposit",
		Reference:      providerReference,
		IdempotencyKey: "employer_funding:" + providerReference,
		Entries: []EntryInput{
			{AccountID: cash.ID, Direction: Debit, Amount: amount},
			{AccountID: funding.ID, Direction: Credit, Amount: amount},
		},
	})
	return err
}

// FundingExposure returns how much more has been advanced than deposited for
// an org, in the given currency: cash_settlement's own balance. Zero or
// negative means deposits cover (or exceed) advances disbursed so far;
// positive means the org owes the pool that much before it is caught up.
func FundingExposure(tx *gorm.DB, orgID string, currency money.Currency) (money.Money, error) {
	cash, err := EnsureAccount(tx, orgID, "", AccountCashSettlement, currency)
	if err != nil {
		return money.Money{}, err
	}
	return AccountBalance(tx, cash.ID)
}
