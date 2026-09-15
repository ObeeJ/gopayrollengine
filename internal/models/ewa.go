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

	// RequireFundingCoverage gates approval on the org having deposited at
	// least as much as it has disbursed (see models.FundingExposure). Off by
	// default: every org running EWA before migration 000019 has zero
	// deposits on record, and flipping this on unconditionally would block
	// all of them from any further draw the moment it ships.
	RequireFundingCoverage bool `gorm:"column:require_funding_coverage;default:false" json:"require_funding_coverage"`

	// CounsellingResourceName / CounsellingContact — the employer's own
	// financial counselling resource (an Employee Assistance Program or
	// equivalent), surfaced to a worker at the Strained tier and above.
	// Empty by default: no third-party service is ever named on the
	// employer's behalf without their say-so.
	CounsellingResourceName string `gorm:"column:counselling_resource_name;default:''" json:"counselling_resource_name"`
	CounsellingContact      string `gorm:"column:counselling_contact;default:''" json:"counselling_contact"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (EWAPolicy) TableName() string { return "ewa_policies" }

// defaultEWAPolicyMajorUnits are illustrative starting points for
// AbsoluteCapKobo/MinDrawKobo/EmergencyFloorKobo, in each currency's own
// MAJOR unit (Naira, Dollars, Cedis, ...) — not derived from a live FX rate.
// A ₦200,000 cap applied verbatim as "200000 minor units" to a GHS or KES
// org would be meaningless in that org's own currency; this at least starts
// a new org's defaults at roughly the right order of magnitude in its own
// money rather than silently reusing Naira's. Ops still tunes real numbers
// per org via UpdatePolicy exactly as for an NGN org — these are a starting
// point, not a considered target.
var defaultEWAPolicyMajorUnits = map[money.Currency]struct{ cap, min, floor int64 }{
	money.NGN: {200_000, 1_000, 5_000},
	money.USD: {500, 5, 20},
	money.GHS: {3_000, 30, 150},
	money.KES: {30_000, 300, 1_500},
	money.ZAR: {4_000, 50, 200},
	money.XOF: {150_000, 1_500, 7_500},
	money.GBP: {400, 4, 15},
	money.EUR: {450, 5, 18},
}

// DefaultEWAPolicy — the values an org gets before anyone tunes anything, in
// the org's own operating currency (see Organization.Currency).
//
// 30% matches what Nigerian employee cooperatives already offer as a salary
// advance with payroll recovery, so it lands on a number workers recognise
// rather than one invented here. It is far easier to loosen a policy after
// watching real usage than to claw back access people have built a budget on.
func DefaultEWAPolicy(orgID string, currency money.Currency) EWAPolicy {
	amounts, ok := defaultEWAPolicyMajorUnits[currency]
	if !ok {
		// Should never happen — the DB CHECK constraint on
		// organizations.currency (migration 000029) already restricts it to
		// this exact set — but fall back to NGN's currency too, not just its
		// amounts: FromMajor below would otherwise fail on the unrecognised
		// code and silently zero out every default.
		amounts = defaultEWAPolicyMajorUnits[money.NGN]
		currency = money.NGN
	}
	cap, _ := money.FromMajor(amounts.cap, currency)
	min, _ := money.FromMajor(amounts.min, currency)
	floor, _ := money.FromMajor(amounts.floor, currency)
	return EWAPolicy{
		OrganizationID:     orgID,
		Enabled:            true,
		MaxAccrualPct:      30,
		AbsoluteCapKobo:    money.Kobo(cap.Minor),
		MinDrawKobo:        money.Kobo(min.Minor),
		MaxDrawsPerPeriod:  4,
		CoolingOffHours:    24,
		EmergencyFloorKobo: money.Kobo(floor.Minor),
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

	// RecoveredKobo is how much of AmountKobo payroll has actually withheld so
	// far. A Disbursed advance whose RecoveredKobo is less than AmountKobo
	// still owes RemainingKobo() — settlement recovers what a given payroll
	// run's net pay can cover and leaves the rest outstanding (still
	// Disbursed) rather than either blocking the whole payroll batch or
	// paying the worker a negative amount. Only reaching RecoveredKobo ==
	// AmountKobo transitions the advance to Settled.
	RecoveredKobo money.Kobo `gorm:"column:recovered_kobo;type:bigint;default:0" json:"recovered_kobo"`

	// Decision evidence, frozen at request time so the decision stays auditable.
	AccruedAtRequestKobo   money.Kobo     `gorm:"column:accrued_at_request_kobo;type:bigint" json:"accrued_at_request_kobo"`
	AvailableAtRequestKobo money.Kobo     `gorm:"column:available_at_request_kobo;type:bigint" json:"available_at_request_kobo"`
	DependencyScore        int            `json:"dependency_score"`
	DependencyTier         DependencyTier `json:"dependency_tier"`
	DeclineReason          string         `json:"decline_reason,omitempty"`

	IdempotencyKey       *string `json:"-"`
	SettledPayrollItemID *string `json:"settled_payroll_item_id,omitempty"`

	// ProviderName / ProviderReference record which payment rail handled this
	// advance and that rail's own identifier for the transfer. Set once
	// InitiateTransfer is accepted; used to correlate a webhook or status poll
	// back to this row, and to route reconciliation through the same
	// provider that submitted it (see provider.Registry.ByName).
	ProviderName      *string `json:"provider_name,omitempty"`
	ProviderReference *string `json:"provider_reference,omitempty"`

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

// RemainingKobo is how much of this advance is still owed — AmountKobo minus
// whatever settlement has already recovered. Zero once fully settled.
func (a *EWAAdvance) RemainingKobo() money.Kobo {
	remaining, err := a.AmountKobo.Sub(a.RecoveredKobo)
	if err != nil || remaining.IsNegative() {
		return 0
	}
	return remaining
}

// EWAAccrualSnapshot — a dated record of what a worker had earned, kept so an
// eligibility decision can be reconstructed during a dispute. WageType and
// HourlyRateKobo (migration 000020) record which basis the snapshot used —
// SalaryKobo alone is meaningless for an hourly employee, who doesn't use
// that column at all (see Employee.WageType).
type EWAAccrualSnapshot struct {
	ID             int64      `gorm:"primaryKey;autoIncrement" json:"id"`
	OrganizationID string     `gorm:"index;not null" json:"organization_id"`
	EmployeeID     string     `gorm:"index;not null" json:"employee_id"`
	Period         string     `gorm:"not null" json:"period"`
	AsOfDate       time.Time  `gorm:"type:date;not null" json:"as_of_date"`
	AccruedKobo    money.Kobo `gorm:"column:accrued_kobo;type:bigint" json:"accrued_kobo"`
	SalaryKobo     money.Kobo `gorm:"column:salary_kobo;type:bigint" json:"salary_kobo"`

	WageType       WageType   `gorm:"column:wage_type;type:text;default:salaried" json:"wage_type"`
	HourlyRateKobo money.Kobo `gorm:"column:hourly_rate_kobo;type:bigint;default:0" json:"hourly_rate_kobo"`

	CreatedAt time.Time `json:"created_at"`
}

func (EWAAccrualSnapshot) TableName() string { return "ewa_accrual_snapshots" }

// EWAWorkerPreference — a worker's own guardrail, set by them rather than
// imposed on them.
//
// ProtectedPaydayMinor is the amount of the next payday the worker has asked the
// system to keep out of reach. It is applied as a cap on top of every policy and
// tier limit, and it is the one guardrail whose legitimacy does not depend on
// the dependency model being right.
type EWAWorkerPreference struct {
	OrganizationID string `gorm:"primaryKey" json:"organization_id"`
	EmployeeID     string `gorm:"primaryKey" json:"employee_id"`

	ProtectedPaydayMinor int64 `gorm:"column:protected_payday_minor" json:"protected_payday_minor"`
	// LastChangedAt is set on every write, raise or lower alike. A lowering
	// attempt is rate-limited against it — not against "last time it was
	// lowered" — so the first lowering after a raise is still protected.
	LastChangedAt *time.Time `json:"last_changed_at,omitempty"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (EWAWorkerPreference) TableName() string { return "ewa_worker_preferences" }

// ProtectedPayday returns the worker's self-imposed floor as Kobo.
func (p EWAWorkerPreference) ProtectedPayday() money.Kobo {
	return money.Kobo(p.ProtectedPaydayMinor)
}

// SavingsMode — how much of net pay automated savings diverts each run.
type SavingsMode string

const (
	// SavingsFixedPercent diverts a flat share of net pay every run.
	SavingsFixedPercent SavingsMode = "fixed_percent"
	// SavingsRoundUp diverts only the "spare change" above the nearest
	// RoundUpToKobo unit — a run that already lands on the unit saves
	// nothing, by design.
	SavingsRoundUp SavingsMode = "round_up"
)

// EWASavingsPreference — a worker's automated-savings election. Opt-in and
// worker-controlled, the same way EWAWorkerPreference's protected-payday
// floor is: the system diverts money at their instruction, never on its own
// judgement about what they should be saving.
type EWASavingsPreference struct {
	OrganizationID string `gorm:"primaryKey" json:"organization_id"`
	EmployeeID     string `gorm:"primaryKey" json:"employee_id"`

	Enabled bool        `gorm:"default:false" json:"enabled"`
	Mode    SavingsMode `gorm:"default:fixed_percent" json:"mode"`

	// FixedPercent — whole percentage points of net pay, FixedPercent mode only.
	FixedPercent int `gorm:"default:0" json:"fixed_percent"`
	// RoundUpToKobo — the round-up unit, RoundUp mode only. Zero means
	// unconfigured: round-up mode with this at zero diverts nothing rather
	// than dividing by zero.
	RoundUpToKobo money.Kobo `gorm:"column:round_up_to_kobo;type:bigint;default:0" json:"round_up_to_kobo"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (EWASavingsPreference) TableName() string { return "ewa_savings_preferences" }

// EWABill — a worker's recorded recurring bill. Lets the product name a
// timing mismatch explicitly (a bill due before payday, when the wages to
// cover it are already earned but not yet paid) instead of treating every
// early draw as a shortfall.
type EWABill struct {
	ID             string `gorm:"primaryKey" json:"id"`
	OrganizationID string `gorm:"index;not null" json:"organization_id"`
	EmployeeID     string `gorm:"index;not null" json:"employee_id"`

	Name       string     `gorm:"not null" json:"name"`
	AmountKobo money.Kobo `gorm:"column:amount_kobo;type:bigint;not null" json:"amount_kobo"`
	// DueDay — day of the month, 1-31. Resolved against each month's actual
	// length at query time (day 31 in a 30-day month means that month's
	// last day), never stored differently per month.
	DueDay int  `gorm:"column:due_day;not null" json:"due_day"`
	Active bool `gorm:"default:true" json:"active"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (EWABill) TableName() string { return "ewa_bills" }

// BeforeCreate — BILL- prefix, consistent with the rest of the ID namespace.
func (b *EWABill) BeforeCreate(tx *gorm.DB) error {
	if b.ID == "" {
		b.ID = "BILL-" + uuid.New().String()[:8]
	}
	return nil
}

// EWAHardshipGrant — a genuine alternative to a fourth advance: discretionary
// employer money issued to a worker outright, never recovered from a future
// payroll run. Recorded here as a decision an admin actually made, the same
// reason employee termination and payroll creation are admin-only actions.
type EWAHardshipGrant struct {
	ID             string `gorm:"primaryKey" json:"id"`
	OrganizationID string `gorm:"index;not null" json:"organization_id"`
	EmployeeID     string `gorm:"index;not null" json:"employee_id"`

	AmountKobo   money.Kobo `gorm:"column:amount_kobo;type:bigint;not null" json:"amount_kobo"`
	Reason       string     `gorm:"not null" json:"reason"`
	ApprovedByIP string     `gorm:"column:approved_by_ip;not null" json:"-"`

	DisbursedAt time.Time `json:"disbursed_at"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

func (EWAHardshipGrant) TableName() string { return "ewa_hardship_grants" }

// BeforeCreate — GRANT- prefix, consistent with the rest of the ID namespace.
func (g *EWAHardshipGrant) BeforeCreate(tx *gorm.DB) error {
	if g.ID == "" {
		g.ID = "GRANT-" + uuid.New().String()[:8]
	}
	if g.DisbursedAt.IsZero() {
		g.DisbursedAt = time.Now()
	}
	return nil
}
