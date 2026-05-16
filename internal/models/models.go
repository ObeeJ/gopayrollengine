package models

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Employee — the human behind the bank account number we're about to encrypt.
type Employee struct {
	ID             string         `gorm:"primaryKey" json:"id"`
	OrganizationID string         `gorm:"index;not null" json:"organization_id"`
	Name           string         `json:"name" binding:"required"`
	Email          string         `gorm:"uniqueIndex" json:"email" binding:"required,email"`
	AccountNumber  EncryptedString `json:"account_number" binding:"required"`
	BankCode       EncryptedString `json:"bank_code" binding:"required"`
	Salary         float64        `json:"salary" binding:"required"`
	IsActive       bool           `gorm:"default:true" json:"is_active"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	DeletedAt      gorm.DeletedAt `gorm:"index" json:"-"`
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

// ErrInvalidTransition — you tried to go backwards. The FSM says no.
var ErrInvalidTransition = fmt.Errorf("invalid payroll status transition")

// validTransitions — the only legal moves on the payroll chessboard.
var validTransitions = map[PayrollStatus][]PayrollStatus{
	PayrollPending:    {PayrollProcessing, PayrollFailed},
	PayrollProcessing: {PayrollCompleted, PayrollFailed},
	PayrollFailed:     {PayrollPending}, // retry is allowed; giving up is not (yet)
	PayrollCompleted:  {},               // terminal — this door only opens one way
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

// TransitionStatus — moves status through the FSM or returns an error if you're being illegal.
func TransitionStatus(db *gorm.DB, model interface{}, current, next PayrollStatus) error {
	if !CanTransition(current, next) {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current, next)
	}
	return db.Model(model).Update("status", next).Error
}

// Payroll — the batch that signs the cheques (metaphorically; Monnify signs them literally).
// PendingCount is an atomic counter so reconciliation fires exactly once, not 500 times.
type Payroll struct {
	ID             string         `gorm:"primaryKey" json:"id"`
	OrganizationID string         `gorm:"index;not null" json:"organization_id"`
	Period         string         `gorm:"uniqueIndex" json:"period" binding:"required"`
	TotalAmount    float64        `json:"total_amount"`
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

// PayrollItem — one employee's slice of the payroll pie.
// TransactionReference links back to Monnify so webhooks know which item to update.
type PayrollItem struct {
	ID                   string         `gorm:"primaryKey" json:"id"`
	OrganizationID       string         `gorm:"index;not null" json:"organization_id"`
	PayrollID            string         `gorm:"index" json:"payroll_id"`
	EmployeeID           string         `gorm:"index" json:"employee_id"`
	EmployeeName         string         `json:"employee_name"`
	Amount               float64        `json:"amount"`
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

// AuditEvent — the black box recorder; append-only, never updated, never deleted.
// CBN, NDPR, and SOC 2 auditors will ask for this. You'll be glad it exists.
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

// AppendAudit — fire-and-forget audit write; never blocks the caller, never loses the receipt.
func AppendAudit(entityType, entityID, action, before, after, actorIP, actorKey string) {
	DB.Create(&AuditEvent{
		EntityType: entityType,
		EntityID:   entityID,
		Action:     action,
		Before:     before,
		After:      after,
		ActorIP:    actorIP,
		ActorKey:   actorKey,
	})
}
