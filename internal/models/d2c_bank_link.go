package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// D2CBankLinkStatus — whether a linked account is still authorized to read
// from. Deliberately simple: a link is either usable or it isn't, the same
// posture TimeEntryStatus takes toward its own smaller state space.
type D2CBankLinkStatus string

const (
	D2CBankLinkLinked  D2CBankLinkStatus = "linked"
	D2CBankLinkRevoked D2CBankLinkStatus = "revoked"
)

// D2CBankLink records that a direct-to-consumer worker has linked a bank
// account for payday prediction (see services.PredictNextPayday) — never
// account numbers, balances, or transaction data, only enough to re-request
// that account's history from the provider later. See migration 000030's
// comment for why this exists and why it stays this narrow.
type D2CBankLink struct {
	ID             string `gorm:"primaryKey" json:"id"`
	OrganizationID string `gorm:"index;not null" json:"organization_id"`
	EmployeeID     string `gorm:"index;not null" json:"employee_id"`

	Provider           string            `gorm:"not null" json:"provider"`
	ProviderAccountRef string            `gorm:"column:provider_account_ref;not null" json:"provider_account_ref"`
	Status             D2CBankLinkStatus `gorm:"default:linked" json:"status"`

	// DebitMandateRef / DebitAuthorizedAt — set only once the worker
	// explicitly authorizes this account to be debited, a separate step
	// from linking it (see migration 000031's comment). Nil means this link
	// can be read from for payday prediction but nothing may be pulled from
	// it yet.
	DebitMandateRef   *string    `gorm:"column:debit_mandate_ref" json:"debit_mandate_ref,omitempty"`
	DebitAuthorizedAt *time.Time `gorm:"column:debit_authorized_at" json:"debit_authorized_at,omitempty"`

	LinkedAt     time.Time  `json:"linked_at"`
	RevokedAt    *time.Time `json:"revoked_at,omitempty"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (D2CBankLink) TableName() string { return "d2c_bank_links" }

// BeforeCreate — D2CBL- prefix, consistent with the rest of the ID namespace.
func (l *D2CBankLink) BeforeCreate(tx *gorm.DB) error {
	if l.ID == "" {
		l.ID = "D2CBL-" + uuid.New().String()[:8]
	}
	return nil
}
