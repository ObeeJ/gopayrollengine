package services

import (
	"context"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
)

// PayslipState is a payment's state in a worker's words. The payroll item's
// own status is an internal state machine; a worker needs to know whether the
// money has arrived, is on its way, or bounced.
type PayslipState string

const (
	PayslipPaid     PayslipState = "paid"
	PayslipOnItsWay PayslipState = "on_its_way"
	PayslipFailed   PayslipState = "failed"
)

// Payslip is one payroll line as the worker it belongs to sees it. It is a
// purpose-built view, not the payroll item: a worker must not see the
// attempt counter, an admin's resolution notes, or provider error text.
type Payslip struct {
	ID        string       `json:"id"`
	PayrollID string       `json:"payroll_id"`
	Period    string       `json:"period"`
	State     PayslipState `json:"state"`
	// Net is what is paid out: gross, less advances recovered, less savings.
	Net money.Kobo `json:"net"`
	// The breakdown is absent (not zero) for lines created before it was
	// recorded; BreakdownAvailable says which.
	BreakdownAvailable bool        `json:"breakdown_available"`
	Gross              *money.Kobo `json:"gross,omitempty"`
	AdvancesDeducted   *money.Kobo `json:"advances_deducted,omitempty"`
	Savings            *money.Kobo `json:"savings,omitempty"`
	// PaidAt is when the bank confirmed the money landed.
	PaidAt    *time.Time `json:"paid_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
}

type payslipRow struct {
	models.PayrollItem
	Period string
}

// ListPayslips returns one worker's payroll lines, newest first. It filters by
// the employee ID from the caller's token (never from the request), and by
// organization as well as RLS, so it stays correct under a role that bypasses
// row-level security.
func (s *PayrollService) ListPayslips(ctx context.Context, orgID, employeeID string, page, pageSize int) ([]Payslip, int64, error) {
	var slips []Payslip
	var total int64
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		if err := tx.Model(&models.PayrollItem{}).
			Where("organization_id = ? AND employee_id = ?", orgID, employeeID).
			Count(&total).Error; err != nil {
			return err
		}
		var rows []payslipRow
		if err := tx.Table("payroll_items AS i").
			Select("i.*, p.period AS period").
			Joins("JOIN payrolls p ON p.id = i.payroll_id AND p.organization_id = i.organization_id").
			Where("i.organization_id = ? AND i.employee_id = ? AND i.deleted_at IS NULL", orgID, employeeID).
			Order("i.created_at DESC, i.id DESC").
			Limit(pageSize).Offset((page - 1) * pageSize).
			Scan(&rows).Error; err != nil {
			return err
		}
		slips = make([]Payslip, 0, len(rows))
		for _, r := range rows {
			slips = append(slips, toPayslip(r))
		}
		return nil
	})
	return slips, total, err
}

func toPayslip(r payslipRow) Payslip {
	p := Payslip{
		ID: r.ID, PayrollID: r.PayrollID, Period: r.Period, Net: r.Amount,
		Gross: r.GrossKobo, AdvancesDeducted: r.AdvancesDeductedKobo, Savings: r.SavingsKobo,
		BreakdownAvailable: r.GrossKobo != nil,
		CreatedAt:          r.CreatedAt,
	}
	switch r.Status {
	case models.PayrollCompleted:
		p.State = PayslipPaid
		p.PaidAt = r.SettledAt
	case models.PayrollFailed:
		p.State = PayslipFailed
	default: // pending (queued), processing (with the bank)
		p.State = PayslipOnItsWay
	}
	return p
}
