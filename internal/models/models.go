package models

import (
	"fmt"
	"time"

	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Employee — encrypted PII with an HMAC blind-index on email for per-org uniqueness.
type Employee struct {
	ID             string          `gorm:"primaryKey" json:"id"`
	OrganizationID string          `gorm:"index;not null" json:"organization_id"`
	Name           string          `json:"name" binding:"required"`
	Email          EncryptedString `json:"email" binding:"required"`
	EmailHMAC      []byte          `gorm:"type:bytea;column:email_hmac" json:"-"`
	AccountNumber  EncryptedString `json:"account_number" binding:"required"`
	BankCode       EncryptedString `json:"bank_code" binding:"required"`
	Salary         money.Kobo      `gorm:"type:bigint;not null;default:0" json:"salary" binding:"required"`
	IsActive       bool            `gorm:"default:true" json:"is_active"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	DeletedAt      gorm.DeletedAt  `gorm:"index" json:"-"`
}

// BeforeSave — keeps EmailHMAC in sync with the plaintext Email before encryption.
func (e *Employee) BeforeSave(tx *gorm.DB) error {
	e.EmailHMAC = BlindIndex(string(e.Email))
	return nil
}

// BeforeCreate — gives every employee a readable ID before GORM touches the DB.
func (e *Employee) BeforeCreate(tx *gorm.DB) (err error) {
	if e.ID == "" {
		e.ID = "EMP-" + uuid.New().String()[:8]
	}
	return
}

// PayrollStatus — the four moods of a payroll batch.
type PayrollStatus string

const (
	PayrollPending    PayrollStatus = "pending"    // born but not yet working
	PayrollProcessing PayrollStatus = "processing" // in the hands of Monnify now
	PayrollCompleted  PayrollStatus = "completed"  // money moved, everyone happy
	PayrollFailed     PayrollStatus = "failed"     // something went wrong, fix it
)

// ErrInvalidTransition — the requested FSM move isn't on the graph; caller logic is wrong.
var ErrInvalidTransition = fmt.Errorf("invalid payroll status transition")

// ErrStaleStatus — legal move, but a concurrent writer beat us; treat as idempotent success.
var ErrStaleStatus = fmt.Errorf("stale status: row already transitioned by a concurrent writer")

// validTransitions — the only legal FSM edges; failed→processing exists so Asynq retries don't dead-letter.
var validTransitions = map[PayrollStatus][]PayrollStatus{
	PayrollPending:    {PayrollProcessing, PayrollFailed},
	PayrollProcessing: {PayrollCompleted, PayrollFailed},
	PayrollFailed:     {PayrollPending, PayrollProcessing},
	PayrollCompleted:  {}, // terminal — this door only opens one way
}

// CanTransition — bouncer at the FSM door; checks the guest list before letting anyone in.
func CanTransition(from, to PayrollStatus) bool {
	for _, allowed := range validTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// TransitionStatus — CAS UPDATE pinned to the expected current status; concurrent writers lose with ErrStaleStatus.
func TransitionStatus(db *gorm.DB, model interface{}, current, next PayrollStatus) error {
	if !CanTransition(current, next) {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current, next)
	}
	res := db.Model(model).Where("status = ?", current).Update("status", next)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: expected %s → %s", ErrStaleStatus, current, next)
	}
	return nil
}

// Payroll — the batch; PendingCount is the atomic counter that makes reconciliation fire exactly once.
type Payroll struct {
	ID             string         `gorm:"primaryKey" json:"id"`
	OrganizationID string         `gorm:"index;not null" json:"organization_id"`
	Period         string         `gorm:"uniqueIndex" json:"period" binding:"required"`
	TotalAmount    money.Kobo     `gorm:"type:bigint;not null;default:0" json:"total_amount"`
	Status         PayrollStatus  `gorm:"default:pending" json:"status"`
	PendingCount   int            `json:"pending_count"`
	Items          []PayrollItem  `json:"items"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `gorm:"index" json:"-"`
}

// BeforeCreate — stamps every payroll with a PAY- prefix so logs are human-readable.
func (p *Payroll) BeforeCreate(tx *gorm.DB) (err error) {
	if p.ID == "" {
		p.ID = "PAY-" + uuid.New().String()[:8]
	}
	return
}

// PayrollItem — one employee's slice; TransactionReference is the webhook's join key.
type PayrollItem struct {
	ID                   string         `gorm:"primaryKey" json:"id"`
	OrganizationID       string         `gorm:"index;not null" json:"organization_id"`
	PayrollID            string         `gorm:"index" json:"payroll_id"`
	EmployeeID           string         `gorm:"index" json:"employee_id"`
	EmployeeName         string         `json:"employee_name"`
	Amount               money.Kobo     `gorm:"type:bigint;not null;default:0" json:"amount"`
	Status               PayrollStatus  `gorm:"default:pending" json:"status"`
	TransactionReference string         `json:"transaction_reference"`
	ErrorMessage         string         `json:"error_message"`
	CreatedAt            time.Time      `json:"created_at"`
	UpdatedAt            time.Time      `json:"updated_at"`
	DeletedAt            gorm.DeletedAt `gorm:"index" json:"-"`
}

// BeforeCreate — ITEM- prefix so you can tell at a glance what kind of record you're looking at.
func (pi *PayrollItem) BeforeCreate(tx *gorm.DB) (err error) {
	if pi.ID == "" {
		pi.ID = "ITEM-" + uuid.New().String()[:8]
	}
	return
}

// AuditEvent — append-only black box; CBN, NDPR, and SOC 2 will ask for it.
type AuditEvent struct {
	ID             string    `gorm:"primaryKey" json:"id"`
	OrganizationID string    `gorm:"index" json:"organization_id"`
	EntityType     string    `gorm:"index:idx_audit_entity" json:"entity_type"`
	EntityID       string    `gorm:"index:idx_audit_entity" json:"entity_id"`
	Action         string    `json:"action"`
	Before         string    `json:"before"`
	After          string    `json:"after"`
	ActorIP        string    `json:"actor_ip"`
	ActorKey       string    `json:"actor_key"` // hashed — raw keys never touch the log
	CreatedAt      time.Time `gorm:"index" json:"created_at"`
}

// BeforeCreate — AUD- prefix; because "audit" deserves its own namespace.
func (a *AuditEvent) BeforeCreate(tx *gorm.DB) (err error) {
	if a.ID == "" {
		a.ID = "AUD-" + uuid.New().String()[:8]
	}
	return
}

// AppendAuditTx — writes one audit row on tx; empty orgID stores NULL for system-level events.
func AppendAuditTx(tx *gorm.DB, orgID, entityType, entityID, action, before, after, actorIP, actorKey string) error {
	return appendAuditOn(tx, orgID, entityType, entityID, action, before, after, actorIP, actorKey)
}

func appendAuditOn(db *gorm.DB, orgID, entityType, entityID, action, before, after, actorIP, actorKey string) error {
	return db.Create(&AuditEvent{
		OrganizationID: orgID,
		EntityType:     entityType,
		EntityID:       entityID,
		Action:         action,
		Before:         before,
		After:          after,
		ActorIP:        actorIP,
		ActorKey:       actorKey,
	}).Error
}
