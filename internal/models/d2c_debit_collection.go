package models

import (
	"time"

	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// D2CDebitCollectionStatus — one attempt to pull a D2C advance's outstanding
// balance from the worker's linked bank account. Mirrors AdvanceStatus's
// posture on acceptance vs. confirmation: 'pending' means the provider
// accepted the request, not that funds moved.
type D2CDebitCollectionStatus string

const (
	D2CCollectionPending    D2CDebitCollectionStatus = "pending"
	D2CCollectionSuccessful D2CDebitCollectionStatus = "successful"
	D2CCollectionFailed     D2CDebitCollectionStatus = "failed"
)

// D2CDebitCollection is one row per ATTEMPT to collect a D2C advance via
// direct debit, not one row per advance — see migration 000031's comment
// for why the full attempt history matters. A successful row is what drives
// the Dr cash_settlement / Cr advance_receivable ledger posting, the direct
// analogue of what SettleAdvancesForPayrollItem does on the payroll-funded
// path.
type D2CDebitCollection struct {
	ID             string `gorm:"primaryKey" json:"id"`
	OrganizationID string `gorm:"index;not null" json:"organization_id"`
	EmployeeID     string `gorm:"index;not null" json:"employee_id"`
	AdvanceID      string `gorm:"index;not null" json:"advance_id"`

	AmountKobo    money.Kobo               `gorm:"column:amount_kobo;type:bigint;not null" json:"amount_kobo"`
	AttemptNumber int                      `gorm:"column:attempt_number;not null" json:"attempt_number"`
	ScheduledFor  time.Time                `gorm:"column:scheduled_for;type:date;not null" json:"scheduled_for"`
	Status        D2CDebitCollectionStatus `gorm:"default:pending" json:"status"`
	Provider      string                   `gorm:"not null" json:"provider"`
	ProviderRef   *string                  `gorm:"column:provider_reference" json:"provider_reference,omitempty"`

	AttemptedAt *time.Time `json:"attempted_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

func (D2CDebitCollection) TableName() string { return "d2c_debit_collections" }

// BeforeCreate — D2CDC- prefix, consistent with the rest of the ID namespace.
func (c *D2CDebitCollection) BeforeCreate(tx *gorm.DB) error {
	if c.ID == "" {
		c.ID = "D2CDC-" + uuid.New().String()[:8]
	}
	return nil
}
