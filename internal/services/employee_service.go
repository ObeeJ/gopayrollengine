package services

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/validate"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	// ErrEmployeeInactive — a terminated employee's record is no longer editable.
	ErrEmployeeInactive = errors.New("employee: this employee is no longer active")
	// ErrInvalidEmployeeUpdate wraps a message written for the person making the request.
	ErrInvalidEmployeeUpdate = errors.New("employee: invalid update")
)

func invalidEmployeeUpdate(field, problem string) error {
	return &EmployeeFieldError{Field: field, Problem: problem}
}

// EmployeeFieldError says which field of an update is wrong and why.
type EmployeeFieldError struct {
	Field   string
	Problem string
}

func (e *EmployeeFieldError) Error() string { return e.Field + " " + e.Problem }
func (e *EmployeeFieldError) Unwrap() error { return ErrInvalidEmployeeUpdate }

// EmployeeUpdate is a partial update: a nil field is left as it is.
type EmployeeUpdate struct {
	Name           *string
	Email          *string
	Phone          *string
	AccountNumber  *string
	BankCode       *string
	Salary         *money.Kobo
	HourlyRateKobo *money.Kobo
}

// IsEmpty reports whether the update names no field at all.
func (u EmployeeUpdate) IsEmpty() bool { return u == EmployeeUpdate{} }

// EmployeeService owns changes to an existing employee record.
type EmployeeService struct{}

// NewEmployeeService constructs the service.
func NewEmployeeService() *EmployeeService { return &EmployeeService{} }

// fieldChange is one changed field, described for the audit log.
type fieldChange struct{ field, before, after string }

// UpdateEmployee applies a partial update to an active employee.
//
// The row is locked for the duration (SELECT … FOR UPDATE), so a concurrent
// termination or a second edit can't be overwritten by this one's save of a
// stale copy. The audit entry names the fields that changed with before/after
// values — masked for anything personal, so the audit trail never becomes a
// second copy of bank details. A request that changes nothing succeeds and
// writes no audit row.
//
// Payment details are read at send time, so a corrected account takes effect
// for the next payment, including a retry of one that bounced.
func (s *EmployeeService) UpdateEmployee(
	ctx context.Context, orgID, employeeID string, upd EmployeeUpdate, by Actor,
) (*models.Employee, error) {
	if upd.IsEmpty() {
		return nil, invalidEmployeeUpdate("body", "must name at least one field to change")
	}

	var emp models.Employee
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&emp, "id = ? AND organization_id = ?", employeeID, orgID).Error; err != nil {
			return err
		}
		if !emp.IsActive {
			return ErrEmployeeInactive
		}
		currency, err := models.OrgCurrencyTx(tx, orgID)
		if err != nil {
			return err
		}

		var changes []fieldChange
		change := func(field, before, after string, personal bool) {
			if personal {
				before, after = models.MaskPII(before), models.MaskPII(after)
			}
			changes = append(changes, fieldChange{field, before, after})
		}

		if upd.Name != nil {
			name := strings.TrimSpace(*upd.Name)
			if name == "" || len(name) > 200 {
				return invalidEmployeeUpdate("name", "must be between 1 and 200 characters")
			}
			if name != emp.Name {
				change("name", emp.Name, name, false)
				emp.Name = name
			}
		}
		if upd.Email != nil {
			email := strings.TrimSpace(*upd.Email)
			if !validate.Email(email) {
				return invalidEmployeeUpdate("email", "must be a valid email address")
			}
			if email != emp.Email.String() {
				change("email", emp.Email.String(), email, true)
				emp.Email = models.EncryptedString(email)
			}
		}

		// Bank details are validated as the resulting pair, not field by
		// field: a new account number is only valid with its bank code.
		newAcct, newBank := emp.AccountNumber.String(), emp.BankCode.String()
		if upd.AccountNumber != nil {
			newAcct = strings.TrimSpace(*upd.AccountNumber)
		}
		if upd.BankCode != nil {
			newBank = strings.TrimSpace(*upd.BankCode)
		}
		if upd.AccountNumber != nil || upd.BankCode != nil {
			if msg := validate.BankDetails(currency, newAcct, newBank); msg != "" {
				// BankDetails words its message as "<field> <problem>".
				field, problem, _ := strings.Cut(msg, " ")
				return invalidEmployeeUpdate(field, problem)
			}
			if newAcct != emp.AccountNumber.String() {
				change("account_number", emp.AccountNumber.String(), newAcct, true)
				emp.AccountNumber = models.EncryptedString(newAcct)
			}
			if newBank != emp.BankCode.String() {
				change("bank_code", emp.BankCode.String(), newBank, true)
				emp.BankCode = models.EncryptedString(newBank)
			}
		}

		if upd.Salary != nil {
			if emp.IsHourly() {
				return invalidEmployeeUpdate("salary", "does not apply to an hourly employee; set hourly_rate_kobo")
			}
			if !upd.Salary.IsPositive() {
				return invalidEmployeeUpdate("salary", "must be greater than zero")
			}
			if *upd.Salary != emp.Salary {
				change("salary", emp.Salary.String(), upd.Salary.String(), false)
				emp.Salary = *upd.Salary
			}
		}
		if upd.HourlyRateKobo != nil {
			if !emp.IsHourly() {
				return invalidEmployeeUpdate("hourly_rate_kobo", "does not apply to a salaried employee; set salary")
			}
			if !upd.HourlyRateKobo.IsPositive() {
				return invalidEmployeeUpdate("hourly_rate_kobo", "must be greater than zero")
			}
			if *upd.HourlyRateKobo != emp.HourlyRateKobo {
				change("hourly_rate_kobo", emp.HourlyRateKobo.String(), upd.HourlyRateKobo.String(), false)
				emp.HourlyRateKobo = *upd.HourlyRateKobo
			}
		}

		if upd.Phone != nil {
			phone := strings.TrimSpace(*upd.Phone)
			if !validate.Phone(phone) {
				return invalidEmployeeUpdate("phone", "must be in international format, e.g. +2348012345678")
			}
			var login models.User
			switch err := tx.Where("employee_id = ?", emp.ID).First(&login).Error; {
			case errors.Is(err, gorm.ErrRecordNotFound):
				if err := tx.Create(&models.User{EmployeeID: emp.ID, OrgID: orgID, Phone: phone}).Error; err != nil {
					return err
				}
				change("phone", "", phone, true)
			case err != nil:
				return err
			case login.Phone != phone:
				if err := tx.Model(&models.User{}).Where("id = ?", login.ID).Update("phone", phone).Error; err != nil {
					return err
				}
				change("phone", login.Phone, phone, true)
			}
		}

		if len(changes) == 0 {
			return nil
		}
		// Save recomputes the email blind index (BeforeSave) and re-encrypts
		// the PII columns; the row lock above makes a full save safe.
		if err := tx.Save(&emp).Error; err != nil {
			return err
		}

		before := make([]string, 0, len(changes))
		after := make([]string, 0, len(changes))
		for _, c := range changes {
			before = append(before, c.field+"="+c.before)
			after = append(after, c.field+"="+c.after)
		}
		return models.AppendAuditTx(tx, orgID, "Employee", emp.ID, "updated",
			strings.Join(before, "; "), strings.Join(after, "; "), by.IP, by.Name)
	})
	if err != nil {
		return nil, fmt.Errorf("update employee %s: %w", employeeID, err)
	}
	return &emp, nil
}
