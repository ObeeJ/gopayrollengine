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

// PayrollService — owns payroll business rules; the only direct GORM contact is the RLS-scoped tx.
type PayrollService struct {
	payrollRepo  repository.PayrollRepository
	employeeRepo repository.EmployeeRepository
}

// NewPayrollService — wires up the service with its repository dependencies.
func NewPayrollService(pr repository.PayrollRepository, er repository.EmployeeRepository) *PayrollService {
	return &PayrollService{payrollRepo: pr, employeeRepo: er}
}

// CreatePayroll builds and persists the batch under the org's RLS scope, then queues it for the worker.
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

	// Enqueue after commit; failed enqueue leaves status=pending for retry, payload carries orgID for RLS.
	payload, _ := json.Marshal(map[string]string{"payroll_id": payroll.ID, "org_id": orgID})
	task := asynq.NewTask(workers.TypeProcessPayroll, payload)
	if _, err := workers.Client.Enqueue(task); err != nil {
		return nil, fmt.Errorf("payroll created but queue rejected it: %v", err)
	}

	return &payroll, nil
}

// GetPayroll — loads a batch with its items, RLS-scoped to the caller's org.
func (s *PayrollService) GetPayroll(ctx context.Context, orgID, id string) (*models.Payroll, error) {
	var payroll *models.Payroll
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		p, fetchErr := s.payrollRepo.WithTx(tx).FindWithItems(orgID, id)
		payroll = p
		return fetchErr
	})
	return payroll, err
}
