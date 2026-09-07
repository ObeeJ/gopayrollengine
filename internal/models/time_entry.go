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

// TimeEntry — one logged block of work for an hourly or gig employee. Counts
// toward accrual, EWA eligibility, and payroll only once Status is 'approved':
// a pending entry is a worker's claim, not verified evidence of wages earned.
type TimeEntry struct {
	ID             string `gorm:"primaryKey" json:"id"`
	OrganizationID string `gorm:"index;not null" json:"organization_id"`
	EmployeeID     string `gorm:"index;not null" json:"employee_id"`

	WorkDate      time.Time       `gorm:"type:date;not null" json:"work_date"`
	MinutesWorked int             `gorm:"not null" json:"minutes_worked"`
	Status        TimeEntryStatus `gorm:"default:pending" json:"status"`
	Note          string          `json:"note,omitempty"`

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

// SumApprovedMinutes totals approved minutes worked by an employee within
// `period` (YYYY-MM), counting only entries on or before asOf. The asOf bound
// makes this function do double duty: called with "now" mid-period it gives
// EWA eligibility a partial-period figure that grows as more entries are
// approved, and called after the period has closed (the normal case for a
// payroll run) it naturally returns the whole period's approved hours since
// no work_date beyond the period end can exist yet to be excluded.
func SumApprovedMinutes(tx *gorm.DB, orgID, employeeID, period string, asOf time.Time) (int64, error) {
	start, err := time.ParseInLocation(PeriodLayout, period, asOf.Location())
	if err != nil {
		return 0, fmt.Errorf("invalid period %q: %w", period, err)
	}
	end := start.AddDate(0, 1, 0)

	// work_date is a DATE column with no time-of-day component, so both bounds
	// must be day-aligned too. A sub-day instant bound is ambiguous: Postgres
	// infers an untyped parameter compared against a DATE column as DATE
	// itself, silently truncating the time and turning a same-day upper bound
	// into a same-day equality — which a strict "<" then always fails,
	// excluding today's entries entirely. Rounding up to the start of the next
	// day makes the bound exact regardless of how the driver types it.
	asOfDate := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, asOf.Location())
	upperExclusive := asOfDate.AddDate(0, 0, 1)
	if upperExclusive.After(end) {
		upperExclusive = end
	}

	var total int64
	err = tx.Model(&TimeEntry{}).
		Where("organization_id = ? AND employee_id = ? AND status = ?", orgID, employeeID, TimeEntryApproved).
		Where("work_date >= ? AND work_date < ?", start, upperExclusive).
		Select("COALESCE(SUM(minutes_worked), 0)").
		Scan(&total).Error
	if err != nil {
		return 0, err
	}
	return total, nil
}

// PeriodLayout mirrors services.PeriodLayout ("2006-01"). Duplicated here
// rather than imported to avoid a models → services dependency; both packages
// must agree on this format, and ewa_service_test.go / time_entry tests pin it.
const PeriodLayout = "2006-01"
