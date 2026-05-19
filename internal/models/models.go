package models

import (
	"fmt"
	"time"

	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Employee — the human behind the bank account number we're about to encrypt.
// Email is stored as ciphertext (Wave 2 #2) and indexed via a deterministic
// HMAC digest (EmailHMAC) so equality and uniqueness still work without
// exposing plaintext at rest. Uniqueness is per-organisation (Wave 2 #1) so
// tenant A cannot probe tenant B's employee directory.
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

// BeforeSave keeps EmailHMAC in sync with the plaintext Email value. Runs on
// both create and update, so a tenant who legitimately rotates an employee's
// email never ends up with a stale digest. Computed from the plaintext field
// before GORM's Value() encrypts it.
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

// ErrInvalidTransition — the requested move is not on the FSM graph (e.g.
// completed → pending). Caller logic is wrong.
var ErrInvalidTransition = fmt.Errorf("invalid payroll status transition")

// ErrStaleStatus — the FSM move is legal on the graph but the row's *current*
// status differs from what the caller passed. Caused by a concurrent writer
// transitioning the row first (e.g. two webhooks for the same payroll item).
// Callers that handle webhooks should treat this as idempotent success.
var ErrStaleStatus = fmt.Errorf("stale status: row already transitioned by a concurrent writer")

// validTransitions — the only legal moves on the payroll chessboard.
//
// failed → processing is permitted because Asynq retries the worker task on
// transient failure; reloading the payroll from the DB shows status=failed and
// the retry attempt would dead-letter forever without this edge. failed →
// pending remains permitted for explicit human-initiated retries (e.g. an ops
// dashboard "retry batch" button) that want to re-enter from the top of the
// FSM rather than jump straight back into worker execution.
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

// TransitionStatus moves a row through the FSM using a compare-and-swap UPDATE.
// The WHERE clause pins the row to its expected current status; if a concurrent
// writer has already transitioned it, RowsAffected is zero and ErrStaleStatus
// is returned. This is the only way to make the FSM safe under concurrency —
// a bare UPDATE without WHERE status=current is a TOCTOU hazard.
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

// Payroll — the batch that signs the cheques (metaphorically; Monnify signs them literally).
// PendingCount is an atomic counter so reconciliation fires exactly once, not 500 times.
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

// PayrollItem — one employee's slice of the payroll pie.
// TransactionReference links back to Monnify so webhooks know which item to update.
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

// AppendAuditTx writes one audit record on the given transaction. Pass the
// caller's organisation ID so the row is RLS-bound to the same tenant; an
// empty orgID stores NULL so system-level events remain globally visible.
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
