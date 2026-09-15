package models

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// TimeEntryStatus — the timesheet entry lifecycle. Deliberately simple next to
// AdvanceStatus: there is no "disbursed" equivalent here, because approving an
// entry is the whole event — it either counts toward accrual or it doesn't.
type TimeEntryStatus string

const (
	TimeEntryPending  TimeEntryStatus = "pending"
	TimeEntryApproved TimeEntryStatus = "approved"
	TimeEntryRejected TimeEntryStatus = "rejected"
)

var validTimeEntryTransitions = map[TimeEntryStatus][]TimeEntryStatus{
	TimeEntryPending:  {TimeEntryApproved, TimeEntryRejected},
	TimeEntryApproved: {},
	TimeEntryRejected: {},
}

// CanTransitionTimeEntry reports whether the FSM edge exists.
func CanTransitionTimeEntry(from, to TimeEntryStatus) bool {
	for _, allowed := range validTimeEntryTransitions[from] {
		if allowed == to {
			return true
		}
	}
	return false
}

// TimeEntryShiftType — which pay differential, if any, applies to an entry's
// hours. Self-declared by the worker at submission: work_date is a DATE with
// no time-of-day, so there is no way to infer "night" from it, and "holiday"
// depends on a calendar this table doesn't have. Like minutes_worked itself,
// it is a claim until an admin approves the entry.
type TimeEntryShiftType string

const (
	ShiftRegular TimeEntryShiftType = "regular"
	ShiftNight   TimeEntryShiftType = "night"
	ShiftWeekend TimeEntryShiftType = "weekend"
	ShiftHoliday TimeEntryShiftType = "holiday"
)

// TimeEntry — one logged block of work for an hourly or gig employee. Counts
// toward accrual, EWA eligibility, and payroll only once Status is 'approved':
// a pending entry is a worker's claim, not verified evidence of wages earned.
type TimeEntry struct {
	ID             string `gorm:"primaryKey" json:"id"`
	OrganizationID string `gorm:"index;not null" json:"organization_id"`
	EmployeeID     string `gorm:"index;not null" json:"employee_id"`

	WorkDate      time.Time          `gorm:"type:date;not null" json:"work_date"`
	MinutesWorked int                `gorm:"not null" json:"minutes_worked"`
	ShiftType     TimeEntryShiftType `gorm:"column:shift_type;default:regular" json:"shift_type"`
	Status        TimeEntryStatus    `gorm:"default:pending" json:"status"`
	Note          string             `json:"note,omitempty"`

	// PaidPayrollItemID is set once, atomically, by the payroll run that pays
	// this entry's minutes (models.MarkTimeEntriesPaid) — nil until then.
	// Never cleared or reassigned: see migration 000028.
	PaidPayrollItemID *string `gorm:"column:paid_payroll_item_id" json:"paid_payroll_item_id,omitempty"`

	RejectionReason string     `json:"rejection_reason,omitempty"`
	ApprovedBy      string     `json:"approved_by,omitempty"`
	ApprovedAt      *time.Time `json:"approved_at,omitempty"`

	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `gorm:"index" json:"-"`
}

func (TimeEntry) TableName() string { return "time_entries" }

// BeforeCreate — TE- prefix, consistent with the rest of the ID namespace.
func (t *TimeEntry) BeforeCreate(tx *gorm.DB) error {
	if t.ID == "" {
		t.ID = "TE-" + uuid.New().String()[:8]
	}
	return nil
}

// TransitionTimeEntry — CAS UPDATE pinned to the expected status, mirroring
// TransitionAdvance: RowsAffected == 0 means a concurrent writer (or a second
// approval click) already resolved this entry.
func TransitionTimeEntry(db *gorm.DB, entry *TimeEntry, current, next TimeEntryStatus, actor, reason string) error {
	if !CanTransitionTimeEntry(current, next) {
		return fmt.Errorf("%w: %s → %s", ErrInvalidTransition, current, next)
	}
	now := time.Now()
	updates := map[string]interface{}{"status": next, "updated_at": now}
	switch next {
	case TimeEntryApproved:
		updates["approved_by"] = actor
		updates["approved_at"] = now
	case TimeEntryRejected:
		updates["rejection_reason"] = reason
	}

	res := db.Model(&TimeEntry{}).
		Where("id = ? AND status = ?", entry.ID, current).
		Updates(updates)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return fmt.Errorf("%w: expected %s → %s", ErrStaleStatus, current, next)
	}
	entry.Status = next
	switch next {
	case TimeEntryApproved:
		entry.ApprovedBy = actor
		entry.ApprovedAt = &now
	case TimeEntryRejected:
		entry.RejectionReason = reason
	}
	return nil
}

// periodUpperBound returns the exclusive upper work_date bound for `period`
// (YYYY-MM): the period's own end, capped at asOf's day-plus-one so a run
// before the period closes never reaches into days that haven't happened
// yet. Shared by every entries-through-a-period query — see
// ApprovedEntriesForPeriod and UnpaidApprovedEntriesThrough.
//
// work_date is a DATE column with no time-of-day component, so the bound
// must be day-aligned too. A sub-day instant is ambiguous: Postgres infers
// an untyped parameter compared against a DATE column as DATE itself,
// silently truncating the time and turning a same-day upper bound into a
// same-day equality — which a strict "<" then always fails, excluding
// today's entries entirely. Rounding up to the start of the next day makes
// the bound exact regardless of how the driver types it.
func periodUpperBound(period string, asOf time.Time) (time.Time, error) {
	start, err := time.ParseInLocation(PeriodLayout, period, asOf.Location())
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid period %q: %w", period, err)
	}
	end := start.AddDate(0, 1, 0)

	asOfDate := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, asOf.Location())
	upperExclusive := asOfDate.AddDate(0, 0, 1)
	if upperExclusive.After(end) {
		upperExclusive = end
	}
	return upperExclusive, nil
}

// ApprovedEntriesForPeriod loads approved entries for an employee within
// `period` (YYYY-MM), counting only entries on or before asOf. The asOf bound
// makes this function do double duty: called with "now" mid-period it gives
// EWA eligibility a partial-period view that grows as more entries are
// approved, and called after the period has closed (the normal case for a
// payroll run) it naturally returns the whole period's approved entries since
// no work_date beyond the period end can exist yet to be excluded. Ordered
// oldest first (WorkDate, then ID as a stable tiebreak) for callers that
// attribute minutes in chronological order, such as weekly overtime.
//
// This is period-scoped by design and stays that way: it backs EWA
// eligibility's accrual estimate, which should reflect wages earned this
// period regardless of whether payroll has already paid for them. For
// payroll gross itself, see UnpaidApprovedEntriesThrough.
func ApprovedEntriesForPeriod(tx *gorm.DB, orgID, employeeID, period string, asOf time.Time) ([]TimeEntry, error) {
	start, err := time.ParseInLocation(PeriodLayout, period, asOf.Location())
	if err != nil {
		return nil, fmt.Errorf("invalid period %q: %w", period, err)
	}
	upperExclusive, err := periodUpperBound(period, asOf)
	if err != nil {
		return nil, err
	}

	var entries []TimeEntry
	err = tx.Where("organization_id = ? AND employee_id = ? AND status = ?", orgID, employeeID, TimeEntryApproved).
		Where("work_date >= ? AND work_date < ?", start, upperExclusive).
		Order("work_date ASC, id ASC").
		Find(&entries).Error
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// UnpaidApprovedEntriesThrough loads approved entries that no payroll run has
// ever paid (paid_payroll_item_id IS NULL), with work_date before `period`'s
// end (see periodUpperBound). Unlike ApprovedEntriesForPeriod there is no
// lower bound: an entry approved after its own period's payroll already ran
// is still unpaid, and this is what lets the next run that comes along pick
// it up instead of it being silently dropped forever. Ordered oldest first,
// same as ApprovedEntriesForPeriod.
//
// The only caller is services.ComputeHourlyGross — payroll gross, which must
// eventually pay every approved entry exactly once, unlike EWA accrual
// (ApprovedEntriesForPeriod), which stays period-scoped by design.
func UnpaidApprovedEntriesThrough(tx *gorm.DB, orgID, employeeID, period string, asOf time.Time) ([]TimeEntry, error) {
	upperExclusive, err := periodUpperBound(period, asOf)
	if err != nil {
		return nil, err
	}

	var entries []TimeEntry
	err = tx.Where("organization_id = ? AND employee_id = ? AND status = ? AND paid_payroll_item_id IS NULL",
		orgID, employeeID, TimeEntryApproved).
		Where("work_date < ?", upperExclusive).
		Order("work_date ASC, id ASC").
		Find(&entries).Error
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// PaidMinutesInISOWeek sums minutes already paid by a prior payroll run
// (paid_payroll_item_id IS NOT NULL) for an employee's approved entries
// falling in ISO week (isoYear, isoWeek) — Postgres's ISOYEAR/WEEK EXTRACT
// fields match Go's time.Time.ISOWeek() exactly. ComputeHourlyGross uses
// this to seed a week's running overtime total when a late-swept entry
// belongs to a week a previous run already partly paid: without it, a late
// approval could be underpaid as regular time even though the week's
// overtime threshold was already crossed.
func PaidMinutesInISOWeek(tx *gorm.DB, orgID, employeeID string, isoYear, isoWeek int) (int64, error) {
	var total int64
	err := tx.Model(&TimeEntry{}).
		Where("organization_id = ? AND employee_id = ? AND status = ? AND paid_payroll_item_id IS NOT NULL",
			orgID, employeeID, TimeEntryApproved).
		Where("EXTRACT(ISOYEAR FROM work_date) = ? AND EXTRACT(WEEK FROM work_date) = ?", isoYear, isoWeek).
		Select("COALESCE(SUM(minutes_worked), 0)").
		Scan(&total).Error
	if err != nil {
		return 0, err
	}
	return total, nil
}

// MarkTimeEntriesPaid records that payrollItemID has paid for entryIDs, so no
// later run ever pays them again. Must run in the same transaction as the
// payroll item's own creation — a rollback of one must roll back the other,
// or a failed run could either double-pay or permanently lose these minutes.
func MarkTimeEntriesPaid(tx *gorm.DB, orgID string, entryIDs []string, payrollItemID string) error {
	if len(entryIDs) == 0 {
		return nil
	}
	return tx.Model(&TimeEntry{}).
		Where("organization_id = ? AND id IN ?", orgID, entryIDs).
		Update("paid_payroll_item_id", payrollItemID).Error
}

// SumApprovedMinutes totals approved minutes worked by an employee within
// `period`, counting only entries on or before asOf — see
// ApprovedEntriesForPeriod for the period-boundary rules.
func SumApprovedMinutes(tx *gorm.DB, orgID, employeeID, period string, asOf time.Time) (int64, error) {
	entries, err := ApprovedEntriesForPeriod(tx, orgID, employeeID, period, asOf)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		total += int64(e.MinutesWorked)
	}
	return total, nil
}

// PeriodLayout mirrors services.PeriodLayout ("2006-01"). Duplicated here
// rather than imported to avoid a models → services dependency; both packages
// must agree on this format, and ewa_service_test.go / time_entry tests pin it.
const PeriodLayout = "2006-01"
