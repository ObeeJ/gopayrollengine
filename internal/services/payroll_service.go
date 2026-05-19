package services

import (
	"context"
	"encoding/json"
	"fmt"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/workers"
	"go-payroll-engine/pkg/money"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

// PayrollService — the manager who knows payroll business rules. Delegates
// all DB I/O to repositories; the only direct GORM contact is the explicit
// RLS-scoped transaction owned at this layer so the header + all items
// commit together inside the same tenant scope.
type PayrollService struct {
	payrollRepo  repository.PayrollRepository
	employeeRepo repository.EmployeeRepository
}

// NewPayrollService — wires up the service with its repository dependencies.
func NewPayrollService(pr repository.PayrollRepository, er repository.EmployeeRepository) *PayrollService {
	return &PayrollService{payrollRepo: pr, employeeRepo: er}
}

// CreatePayroll fetches active employees, builds the batch, commits the
// header + every item atomically inside an RLS-scoped transaction, then
// enqueues the worker task. Every DB read and write inside this method runs
// under app.org_id = orgID, so a stale or wrong orgID can never accidentally
// build a payroll out of another tenant's employees.
func (s *PayrollService) CreatePayroll(ctx context.Context, orgID, period string) (*models.Payroll, error) {
	payroll := models.Payroll{
		OrganizationID: orgID,
		Period:         period,
		Status:         models.PayrollPending,
	}

	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		employees, err := s.employeeRepo.WithTx(tx).FindAllActive(orgID)
		if err != nil {
			return err
		}
		if len(employees) == 0 {
			return fmt.Errorf("no active employees found for this organization")
		}

		// Overflow-checked fold over Kobo — never use += on money.
		salaries := make([]money.Kobo, len(employees))
		for i, emp := range employees {
			salaries[i] = emp.Salary
		}
		total, sumErr := money.Sum(salaries)
		if sumErr != nil {
			return fmt.Errorf("payroll total overflow: %w", sumErr)
		}
		payroll.TotalAmount = total

		txPayrollRepo := s.payrollRepo.WithTx(tx)
		if err := txPayrollRepo.Create(&payroll); err != nil {
			return err
		}
		for _, emp := range employees {
			item := models.PayrollItem{
				OrganizationID: orgID,
				PayrollID:      payroll.ID,
				EmployeeID:     emp.ID,
				EmployeeName:   emp.Name,
				Amount:         emp.Salary,
				Status:         models.PayrollPending,
			}
			if err := txPayrollRepo.CreateItem(&item); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Enqueue happens after commit — a failed enqueue leaves the payroll in
	// "pending" for manual or automated retry. The task payload carries the
	// orgID so the worker can re-enter the RLS scope.
	payload, _ := json.Marshal(map[string]string{"payroll_id": payroll.ID, "org_id": orgID})
	task := asynq.NewTask(workers.TypeProcessPayroll, payload)
	if _, err := workers.Client.Enqueue(task); err != nil {
		return nil, fmt.Errorf("payroll created but queue rejected it: %v", err)
	}

	return &payroll, nil
}

// GetPayroll fetches a payroll batch with all its items, RLS-scoped to the
// caller's organisation.
func (s *PayrollService) GetPayroll(ctx context.Context, orgID, id string) (*models.Payroll, error) {
	var payroll *models.Payroll
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		p, fetchErr := s.payrollRepo.WithTx(tx).FindWithItems(orgID, id)
		payroll = p
		return fetchErr
	})
	return payroll, err
}
