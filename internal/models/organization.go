package models

import (
	"time"

	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// Organization — the top of the DAG; every other record hangs off this node.
type Organization struct {
	ID           string `gorm:"primaryKey" json:"id"`
	Name         string `json:"name" binding:"required"`
	PasswordHash string `json:"-"` // bcrypt — never serialised to JSON
	Role         string `gorm:"default:admin" json:"role"`
	DataRegion   string `gorm:"default:ng" json:"data_region"` // "ng" | "eu" | "us" — enforced by DataResidency middleware

	// Currency is the org's operating currency — every employee salary,
	// payroll amount, and EWA advance under this org is denominated in it,
	// and every ledger account this org owns is opened in it. Set implicitly
	// at creation (default NGN) and never changed after: migration 000029's
	// trigger enforces that at the database level, because switching it out
	// from under an org's existing ledger history would corrupt the ledger's
	// per-account currency invariant, not convert anything.
	Currency money.Currency `gorm:"default:NGN" json:"currency"`

	// IsD2C flags a direct-to-consumer org — a worker onboarded without an
	// employer running payroll for them (see migration 000030). Their single
	// Employee record is themself, not staff of a business; EWA eligibility
	// and settlement for this org come from observed bank deposit history
	// and direct debit rather than payroll accrual and deduction. Query this
	// explicitly wherever that distinction matters — don't infer it from the
	// absence of payroll activity.
	IsD2C bool `gorm:"column:is_d2c;default:false" json:"is_d2c"`

	IsActive  bool           `gorm:"default:true" json:"is_active"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

// BeforeCreate — ORG- prefix; consistent with EMP-, PAY-, ITEM- naming convention.
func (o *Organization) BeforeCreate(tx *gorm.DB) (err error) {
	if o.ID == "" {
		o.ID = "ORG-" + uuid.New().String()[:8]
	}
	return
}

// SetPassword — hashes the password with bcrypt cost 12; never store plaintext.
func (o *Organization) SetPassword(plain string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(plain), 12)
	if err != nil {
		return err
	}
	o.PasswordHash = string(hash)
	return nil
}

// OrgCurrencyTx loads an org's operating currency — the single source of
// truth every ledger posting, provider-routing decision, and EWA policy
// default keys off instead of an implicit NGN. A cheap primary-key lookup:
// callers that already hold `tx` from WithOrgScope pay one extra indexed
// read, not a new transaction.
func OrgCurrencyTx(tx *gorm.DB, orgID string) (money.Currency, error) {
	var org Organization
	if err := tx.Select("currency").First(&org, "id = ?", orgID).Error; err != nil {
		return "", err
	}
	return org.Currency, nil
}

// ConsentRecord — NDPR Art. 26; append-only, withdrawal creates a new row.
type ConsentRecord struct {
	ID             string     `gorm:"primaryKey" json:"id"`
	OrganizationID string     `gorm:"index;not null" json:"organization_id"`
	EmployeeID     string     `gorm:"index;not null" json:"employee_id"`
	ConsentType    string     `json:"consent_type"` // "payroll_processing" | "ewa_access" | "data_sharing"
	Granted        bool       `json:"granted"`      // true = consent given, false = withdrawn
	IPAddress      string     `json:"ip_address"`
	UserAgent      string     `json:"user_agent"`
	ConsentedAt    time.Time  `json:"consented_at"`
	ExpiresAt      *time.Time `json:"expires_at,omitempty"` // nil = indefinite
}

// BeforeCreate — CON- prefix; auditors love a consistent namespace.
func (cr *ConsentRecord) BeforeCreate(tx *gorm.DB) (err error) {
	if cr.ID == "" {
		cr.ID = "CON-" + uuid.New().String()[:8]
	}
	return
}

// HasActiveConsent — true if a current, non-expired consent exists; absence means no processing.
func HasActiveConsent(db *gorm.DB, orgID, employeeID, consentType string) bool {
	var count int64
	db.Model(&ConsentRecord{}).
		Where("organization_id = ? AND employee_id = ? AND consent_type = ? AND granted = true AND (expires_at IS NULL OR expires_at > ?)",
			orgID, employeeID, consentType, time.Now()).
		Count(&count)
	return count > 0
}

// BVNVerification — outcome row from a BVN check; stores response hash, never the BVN itself.
type BVNVerification struct {
	ID             string    `gorm:"primaryKey" json:"id"`
	OrganizationID string    `gorm:"index;not null" json:"organization_id"`
	EmployeeID     string    `gorm:"uniqueIndex;not null" json:"employee_id"` // one verification per employee
	Provider       string    `json:"provider"`                                // "dojah" | "smile" | "prembly"
	Status         string    `json:"status"`                                  // "verified" | "failed" | "pending"
	ResponseHash   string    `json:"response_hash"`                           // SHA-256 of provider response — not the BVN
	VerifiedAt     time.Time `json:"verified_at"`
	CreatedAt      time.Time `json:"created_at"`
}

// BeforeCreate — BVN- prefix; keeps the audit trail readable.
func (b *BVNVerification) BeforeCreate(tx *gorm.DB) (err error) {
	if b.ID == "" {
		b.ID = "BVN-" + uuid.New().String()[:8]
	}
	return
}
