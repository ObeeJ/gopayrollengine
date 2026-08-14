package models

import (
	"fmt"
	"time"

	"go-payroll-engine/pkg/money"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// AdvanceStatus — the EWA advance lifecycle.
type AdvanceStatus string

const (
	AdvanceRequested  AdvanceStatus = "requested"
	AdvanceApproved   AdvanceStatus = "approved"
	AdvanceDisbursed  AdvanceStatus = "disbursed"
	AdvanceSettled    AdvanceStatus = "settled" // recovered from payroll
	AdvanceDeclined   AdvanceStatus = "declined"
	AdvanceCancelled  AdvanceStatus = "cancelled"
	AdvanceWrittenOff AdvanceStatus = "written_off"
)

// validAdvanceTransitions — the only legal edges. Note what is absent: there is
// no path from settled or written_off back to anything. Recovery of a written-off
// advance is a new reversing ledger posting, not a status rewind.
var validAdvanceTransitions = map[AdvanceStatus][]AdvanceStatus{
	AdvanceRequested:  {AdvanceApproved, AdvanceDeclined, AdvanceCancelled},
	AdvanceApproved:   {AdvanceDisbursed, AdvanceCancelled},
	AdvanceDisbursed:  {AdvanceSettled, AdvanceWrittenOff},
	AdvanceSettled:    {},
	AdvanceDeclined:   {},
	AdvanceCancelled:  {},
	AdvanceWrittenOff: {},
}

// CanTransitionAdvance reports whether the FSM edge exists.
func CanTransitionAdvance(from, to AdvanceStatus) bool {
	for _, allowed := range validAdvanceTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// TransitionAdvance — CAS UPDATE pinned to the expected status. Mirrors
// TransitionStatus: RowsAffected == 0 means a concurrent writer won.
func TransitionAdvance(db *gorm.DB, advance *EWAAdvance, current, next AdvanceStatus) error {
	if !CanTransitionAdvance(current, next) {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current, next)
	}
	updates := map[string]interface{}{"status": next, "updated_at": time.Now()}
	switch next {
	case AdvanceDisbursed:
		updates["disbursed_at"] = time.Now()
	case AdvanceSettled:
		updates["settled_at"] = time.Now()
	}

	res := db.Model(&EWAAdvance{}).
		Where("id = ? AND status = ?", advance.ID, current).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: expected %s → %s", ErrStaleStatus, current, next)
	}
	advance.Status = next
	return nil
}

// DependencyTier — how much of a worker's pay is being consumed before payday,
// and how entrenched the pattern is.
type DependencyTier string

const (
	TierHealthy   DependencyTier = "healthy"
	TierElevated  DependencyTier = "elevated"
	TierStrained  DependencyTier = "strained"
	TierDependent DependencyTier = "dependent"
)

// EWAPolicy — per-org guardrail configuration.
type EWAPolicy struct {
	OrganizationID string `gorm:"primaryKey" json:"organization_id"`
	Enabled        bool   `gorm:"default:true" json:"enabled"`

	MaxAccrualPct      int        `gorm:"default:50" json:"max_accrual_pct"`
	AbsoluteCapKobo    money.Kobo `gorm:"column:absolute_cap_kobo;type:bigint" json:"absolute_cap_kobo"`
	MinDrawKobo        money.Kobo `gorm:"column:min_draw_kobo;type:bigint" json:"min_draw_kobo"`
	MaxDrawsPerPeriod  int        `gorm:"default:4" json:"max_draws_per_period"`
	CoolingOffHours    int        `gorm:"default:24" json:"cooling_off_hours"`
	EmergencyFloorKobo money.Kobo `gorm:"column:emergency_floor_kobo;type:bigint" json:"emergency_floor_kobo"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (EWAPolicy) TableName() string { return "ewa_policies" }

// DefaultEWAPolicy — the values an org gets before anyone tunes anything.
// Chosen to be conservative: it is far easier to loosen a policy after watching
// real usage than to claw back access people have started depending on.
func DefaultEWAPolicy(orgID string) EWAPolicy {
	return EWAPolicy{
		OrganizationID:     orgID,
		Enabled:            true,
		MaxAccrualPct:      50,
		AbsoluteCapKobo:    money.FromNaira(200_000),
		MinDrawKobo:        money.FromNaira(1_000),
		MaxDrawsPerPeriod:  4,
		CoolingOffHours:    24,
		EmergencyFloorKobo: money.FromNaira(5_000),
	}
}

// EWAAdvance — one early-wage draw against wages already earned.
type EWAAdvance struct {
	ID             string  `gorm:"primaryKey" json:"id"`
	OrganizationID string  `gorm:"index;not null" json:"organization_id"`
	EmployeeID     string  `gorm:"index;not null" json:"employee_id"`
	UserID         *string `json:"user_id,omitempty"`

	Period     string        `gorm:"not null" json:"period"`
	AmountKobo money.Kobo    `gorm:"column:amount_kobo;type:bigint;not null" json:"amount_kobo"`
	FeeKobo    money.Kobo    `gorm:"column:fee_kobo;type:bigint;default:0" json:"fee_kobo"`
	Status     AdvanceStatus `gorm:"default:requested" json:"status"`

	// Decision evidence, frozen at request time so the decision stays auditable.
	AccruedAtRequestKobo   money.Kobo     `gorm:"column:accrued_at_request_kobo;type:bigint" json:"accrued_at_request_kobo"`
	AvailableAtRequestKobo money.Kobo     `gorm:"column:available_at_request_kobo;type:bigint" json:"available_at_request_kobo"`
	DependencyScore        int            `json:"dependency_score"`
	DependencyTier         DependencyTier `json:"dependency_tier"`
	DeclineReason          string         `json:"decline_reason,omitempty"`

	IdempotencyKey       *string `json:"-"`
	SettledPayrollItemID *string `json:"settled_payroll_item_id,omitempty"`

	RequestedAt time.Time      `json:"requested_at"`
	DisbursedAt *time.Time     `json:"disbursed_at,omitempty"`
	SettledAt   *time.Time     `json:"settled_at,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}

func (EWAAdvance) TableName() string { return "ewa_advances" }

// BeforeCreate — EWA- prefix, consistent with the rest of the ID namespace.
func (a *EWAAdvance) BeforeCreate(tx *gorm.DB) error {
	if a.ID == "" {
		a.ID = "EWA-" + uuid.New().String()[:8]
	}
	if a.RequestedAt.IsZero() {
		a.RequestedAt = time.Now()
	}
	return nil
}

// IsOutstanding reports whether this advance still represents money owed.
func (a *EWAAdvance) IsOutstanding() bool {
	return a.Status == AdvanceApproved || a.Status == AdvanceDisbursed
}

// EWAAccrualSnapshot — a dated record of what a worker had earned, kept so an
// eligibility decision can be reconstructed during a dispute.
type EWAAccrualSnapshot struct {
	ID             int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	OrganizationID string     `gorm:"index;not null" json:"organization_id"`
	EmployeeID     string     `gorm:"index;not null" json:"employee_id"`
	Period         string     `gorm:"not null" json:"period"`
	AsOfDate       time.Time  `gorm:"type:date;not null" json:"as_of_date"`
	AccruedKobo    money.Kobo `gorm:"column:accrued_kobo;type:bigint" json:"accrued_kobo"`
	SalaryKobo     money.Kobo `gorm:"column:salary_kobo;type:bigint" json:"salary_kobo"`
	CreatedAt      time.Time  `json:"created_at"`
}

func (EWAAccrualSnapshot) TableName() string { return "ewa_accrual_snapshots" }
