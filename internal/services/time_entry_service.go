package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go-payroll-engine/internal/models"

	"gorm.io/gorm"
)

// Errors returned by TimeEntryService. Each maps to a specific 4xx response
// at the handler layer rather than falling through as a raw DB constraint
// violation.
var (
	ErrTimeEntryNotHourly       = errors.New("time entry: employee is not an hourly worker")
	ErrTimeEntryFutureDate      = errors.New("time entry: work date is in the future")
	ErrTimeEntryInvalidRange    = errors.New("time entry: minutes worked must be between 1 and 1440")
	ErrTimeEntryAlreadyResolved = errors.New("time entry: already approved or rejected")
	ErrTimeEntryInvalidShift    = errors.New("time entry: shift_type must be regular, night, weekend, or holiday")
	ErrTimeEntryDayOverflow     = errors.New("time entry: total minutes for this day would exceed 24 hours")
	ErrTimeEntryInactive        = errors.New("time entry: employee is no longer active")
)

var validShiftTypes = map[models.TimeEntryShiftType]bool{
	models.ShiftRegular: true,
	models.ShiftNight:   true,
	models.ShiftWeekend: true,
	models.ShiftHoliday: true,
}

// maxMinutesPerEntry mirrors the CHECK constraint in migration 000018: a
// single entry longer than 24h is a data error, not a long shift.
const maxMinutesPerEntry = 1440

// lockEmployeeDay serialises every write that changes an employee's minutes
// for one work date, so two concurrent submissions (or approvals) can't each
// pass the 24h check against the same starting total.
func lockEmployeeDay(tx *gorm.DB, employeeID string, day time.Time) error {
	return tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))",
		"time_entry:"+employeeID+":"+day.Format("2006-01-02")).Error
}

// dayMinutes sums an employee's minutes on one work date across statuses.
func dayMinutes(tx *gorm.DB, employeeID string, day time.Time, statuses []models.TimeEntryStatus, excludeID string) (int, error) {
	var total int
	err := tx.Model(&models.TimeEntry{}).
		Where("employee_id = ? AND work_date = ? AND status IN ? AND id <> ?", employeeID, day, statuses, excludeID).
		Select("COALESCE(SUM(minutes_worked), 0)").Scan(&total).Error
	return total, err
}

// TimeEntryService owns the hourly/gig timesheet lifecycle: a worker submits
// hours, an admin approves or rejects them, and only approved minutes ever
// reach accrual (EWAService), payroll (PayrollService), or the dependency
// scoring denominator. See migration 000018 for why unapproved entries must
// never count toward any of those.
type TimeEntryService struct{}

// NewTimeEntryService constructs the service.
func NewTimeEntryService() *TimeEntryService { return &TimeEntryService{} }

// SubmitTimeEntry records a worker's claim of hours worked on a given date,
// with which shift differential (if any) applies. It starts 'pending' and
// counts toward nothing until an admin reviews it.
func (s *TimeEntryService) SubmitTimeEntry(
	ctx context.Context, orgID, employeeID string, workDate time.Time, minutesWorked int,
	shiftType models.TimeEntryShiftType, note string,
) (*models.TimeEntry, error) {
	if minutesWorked <= 0 || minutesWorked > maxMinutesPerEntry {
		return nil, ErrTimeEntryInvalidRange
	}
	if shiftType == "" {
		shiftType = models.ShiftRegular
	}
	if !validShiftTypes[shiftType] {
		return nil, ErrTimeEntryInvalidShift
	}
	day := truncateToDay(workDate)
	if day.After(truncateToDay(time.Now())) {
		return nil, ErrTimeEntryFutureDate
	}

	var entry *models.TimeEntry
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		var emp models.Employee
		if err := tx.First(&emp, "id = ?", employeeID).Error; err != nil {
			return err
		}
		if !emp.IsHourly() {
			return ErrTimeEntryNotHourly
		}
		if !emp.IsActive {
			return ErrTimeEntryInactive
		}

		// Each entry is capped at 24h, but nothing capped the day: several
		// entries for one date could sum past 24h, and once approved they
		// inflate accrual — and so how much the worker can draw.
		if err := lockEmployeeDay(tx, employeeID, day); err != nil {
			return err
		}
		claimed, err := dayMinutes(tx, employeeID, day,
			[]models.TimeEntryStatus{models.TimeEntryPending, models.TimeEntryApproved}, "")
		if err != nil {
			return err
		}
		if claimed+minutesWorked > maxMinutesPerEntry {
			return ErrTimeEntryDayOverflow
		}

		record := models.TimeEntry{
			OrganizationID: orgID,
			EmployeeID:     employeeID,
			WorkDate:       day,
			MinutesWorked:  minutesWorked,
			ShiftType:      shiftType,
			Note:           note,
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		entry = &record
		return models.AppendAuditTx(tx, orgID, "TimeEntry", record.ID, "submitted",
			"", fmt.Sprintf("%d minutes on %s", minutesWorked, day.Format("2006-01-02")), "", "")
	})
	return entry, err
}

// ApproveTimeEntry moves a pending entry to approved, making its minutes
// count toward accrual, payroll, and dependency scoring from this point on.
// actor identifies who approved it for the audit trail; employer-side JWTs
// carry a role but no individual admin identity, so callers typically pass
// the role (e.g. "admin") — the same limit AppendAuditTx already accepts
// elsewhere in this codebase.
func (s *TimeEntryService) ApproveTimeEntry(ctx context.Context, orgID, entryID, actor, actorIP string) (*models.TimeEntry, error) {
	var entry *models.TimeEntry
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		var e models.TimeEntry
		if err := tx.First(&e, "id = ?", entryID).Error; err != nil {
			return err
		}
		// Re-checked at approval, which is what makes minutes count: entries
		// that predate the submit-time check, or were resolved out of order,
		// must still never approve a day past 24h.
		if err := lockEmployeeDay(tx, e.EmployeeID, e.WorkDate); err != nil {
			return err
		}
		approved, err := dayMinutes(tx, e.EmployeeID, e.WorkDate,
			[]models.TimeEntryStatus{models.TimeEntryApproved}, e.ID)
		if err != nil {
			return err
		}
		if approved+e.MinutesWorked > maxMinutesPerEntry {
			return ErrTimeEntryDayOverflow
		}
		if err := models.TransitionTimeEntry(tx, &e, models.TimeEntryPending, models.TimeEntryApproved, actor, ""); err != nil {
			if errors.Is(err, models.ErrStaleStatus) {
				return ErrTimeEntryAlreadyResolved
			}
			return err
		}
		entry = &e
		return models.AppendAuditTx(tx, orgID, "TimeEntry", e.ID, "approved", "pending", "approved", actorIP, actor)
	})
	return entry, err
}

// RejectTimeEntry moves a pending entry to rejected. Rejected entries are
// kept, not deleted — a worker disputing a rejection needs the record to
// still exist.
func (s *TimeEntryService) RejectTimeEntry(ctx context.Context, orgID, entryID, actor, actorIP, reason string) (*models.TimeEntry, error) {
	var entry *models.TimeEntry
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		var e models.TimeEntry
		if err := tx.First(&e, "id = ?", entryID).Error; err != nil {
			return err
		}
		if err := models.TransitionTimeEntry(tx, &e, models.TimeEntryPending, models.TimeEntryRejected, actor, reason); err != nil {
			if errors.Is(err, models.ErrStaleStatus) {
				return ErrTimeEntryAlreadyResolved
			}
			return err
		}
		entry = &e
		return models.AppendAuditTx(tx, orgID, "TimeEntry", e.ID, "rejected", "pending", reason, actorIP, actor)
	})
	return entry, err
}

// ListTimeEntries returns an employee's entries (or, with employeeID empty,
// every employee's entries in the org) newest first. status filters to one
// state; pass "" for all states — the admin review queue uses "pending",
// a worker's own history page uses "".
func (s *TimeEntryService) ListTimeEntries(
	ctx context.Context, orgID, employeeID string, status models.TimeEntryStatus, limit int,
) ([]models.TimeEntry, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	var entries []models.TimeEntry
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		q := tx.Order("work_date desc, created_at desc").Limit(limit)
		if employeeID != "" {
			q = q.Where("employee_id = ?", employeeID)
		}
		if status != "" {
			q = q.Where("status = ?", status)
		}
		return q.Find(&entries).Error
	})
	return entries, err
}
