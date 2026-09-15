package services

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
)

// ErrInvalidBill is returned when a bill fails validation.
var ErrInvalidBill = errors.New("invalid bill")

func invalidBill(reason string) error {
	return fmt.Errorf("%w: %s", ErrInvalidBill, reason)
}

// AddBill records a worker's recurring bill. A worker's own data, entered by
// them — the product makes no claim about which bills are real or important.
func (s *EWAService) AddBill(
	ctx context.Context, orgID, employeeID, name string, amount money.Kobo, dueDay int,
) (*models.EWABill, error) {
	if name == "" {
		return nil, invalidBill("name is required")
	}
	if !amount.IsPositive() {
		return nil, invalidBill("amount must be positive")
	}
	if dueDay < 1 || dueDay > 31 {
		return nil, invalidBill("due_day must be between 1 and 31")
	}

	bill := models.EWABill{
		OrganizationID: orgID,
		EmployeeID:     employeeID,
		Name:           name,
		AmountKobo:     amount,
		DueDay:         dueDay,
		Active:         true,
	}
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		return tx.Create(&bill).Error
	})
	return &bill, err
}

// ListBills returns a worker's active bills.
func (s *EWAService) ListBills(ctx context.Context, orgID, employeeID string) ([]models.EWABill, error) {
	var bills []models.EWABill
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		return tx.Where("employee_id = ? AND active = ?", employeeID, true).
			Order("due_day ASC").Find(&bills).Error
	})
	return bills, err
}

// RemoveBill deactivates a bill. Soft, not deleted outright — the same
// reasoning as everywhere else in this codebase that a financial record
// should never simply vanish.
func (s *EWAService) RemoveBill(ctx context.Context, orgID, employeeID, billID string) error {
	return models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		res := tx.Model(&models.EWABill{}).
			Where("id = ? AND employee_id = ?", billID, employeeID).
			Update("active", false)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
}

// BillTimingEntry is one bill's position relative to the next payday.
type BillTimingEntry struct {
	Bill models.EWABill `json:"bill"`
	// DueDate is this bill's next occurrence, resolved against the actual
	// length of the month it falls in.
	DueDate time.Time `json:"due_date"`
	// Mismatched means this bill falls before the next payday — the wages to
	// cover it may already be earned, just not yet paid.
	Mismatched bool `json:"mismatched"`
	// Coverable means the worker's currently available EWA draw is enough to
	// close this specific gap, after earlier (sooner-due) mismatched bills
	// have already claimed their share of that same availability.
	Coverable bool `json:"coverable"`
}

// BillTimingPlan is the full picture: a worker's recorded bills laid out
// against their payday, so a timing mismatch reads as exactly that — a
// scheduling gap with a precise size — rather than an undifferentiated need.
type BillTimingPlan struct {
	AsOf       time.Time         `json:"as_of"`
	NextPayday time.Time         `json:"next_payday"`
	Available  money.Kobo        `json:"available"`
	Bills      []BillTimingEntry `json:"bills"`
	// TotalDueBeforePayday sums every mismatched bill, regardless of whether
	// it's coverable — the size of the timing gap itself.
	TotalDueBeforePayday money.Kobo `json:"total_due_before_payday"`
	// TotalCoverable sums only the bills a draw today could actually close.
	TotalCoverable money.Kobo `json:"total_coverable"`
}

// GetBillTimingPlan lays a worker's recorded bills out against their next
// payday. Bills due before payday are flagged as a timing mismatch, and each
// is checked against currently available EWA draw capacity — soonest due
// first, since that is the order a worker would actually need to act in —
// to say precisely which of them a draw today would close.
func (s *EWAService) GetBillTimingPlan(
	ctx context.Context, orgID, employeeID string, asOf time.Time,
) (*BillTimingPlan, error) {
	var bills []models.EWABill
	var el *Eligibility
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		if err := tx.Where("employee_id = ? AND active = ?", employeeID, true).
			Find(&bills).Error; err != nil {
			return err
		}
		var elErr error
		el, elErr = s.eligibilityTx(ctx, tx, orgID, employeeID, asOf)
		return elErr
	})
	if err != nil {
		return nil, err
	}

	// nextPayday mirrors outstandingTx/eligibilityTx's own pay-cycle
	// assumption: a calendar-month period paid at month end.
	nextPayday := firstOfNextPeriod(asOf).AddDate(0, 0, -1)

	type dated struct {
		bill    models.EWABill
		dueDate time.Time
	}
	dueList := make([]dated, 0, len(bills))
	for _, b := range bills {
		dueList = append(dueList, dated{bill: b, dueDate: nextBillDueDate(asOf, b.DueDay)})
	}
	sort.Slice(dueList, func(i, j int) bool { return dueList[i].dueDate.Before(dueList[j].dueDate) })

	plan := &BillTimingPlan{AsOf: asOf, NextPayday: nextPayday, Available: el.Available}
	remaining := el.Available
	for _, d := range dueList {
		entry := BillTimingEntry{Bill: d.bill, DueDate: d.dueDate}
		entry.Mismatched = d.dueDate.Before(nextPayday)
		if entry.Mismatched {
			total, addErr := plan.TotalDueBeforePayday.Add(d.bill.AmountKobo)
			if addErr != nil {
				return nil, fmt.Errorf("bill total overflow: %w", addErr)
			}
			plan.TotalDueBeforePayday = total

			if d.bill.AmountKobo <= remaining {
				entry.Coverable = true
				var subErr error
				remaining, subErr = remaining.Sub(d.bill.AmountKobo)
				if subErr != nil {
					return nil, fmt.Errorf("remaining budget underflow: %w", subErr)
				}
				coverable, addErr := plan.TotalCoverable.Add(d.bill.AmountKobo)
				if addErr != nil {
					return nil, fmt.Errorf("coverable total overflow: %w", addErr)
				}
				plan.TotalCoverable = coverable
			}
		}
		plan.Bills = append(plan.Bills, entry)
	}
	return plan, nil
}

// daysInMonth is the number of days in the given month.
func daysInMonth(year int, month time.Month) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// nextBillDueDate resolves a bill's DueDay to its next actual occurrence on
// or after asOf. A DueDay past the end of a shorter month (31 in a 30-day
// month) resolves to that month's last day, never a different stored value.
func nextBillDueDate(asOf time.Time, dueDay int) time.Time {
	y, m := asOf.Year(), asOf.Month()
	day := dueDay
	if maxDay := daysInMonth(y, m); day > maxDay {
		day = maxDay
	}
	candidate := time.Date(y, m, day, 0, 0, 0, 0, asOf.Location())
	today := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, asOf.Location())
	if candidate.Before(today) {
		day = dueDay
		if maxDay := daysInMonth(y, m+1); day > maxDay {
			day = maxDay
		}
		candidate = time.Date(y, m+1, day, 0, 0, 0, 0, asOf.Location())
	}
	return candidate
}
