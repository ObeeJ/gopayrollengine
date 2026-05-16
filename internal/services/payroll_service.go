package services

import (
	"encoding/json"
	"fmt"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/internal/workers"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

// PayrollService — the manager who knows payroll business rules.
// It delegates all DB reads/writes to repositories — it never touches GORM directly.
type PayrollService struct {
	payrollRepo  repository.PayrollRepository
	employeeRepo repository.EmployeeRepository
}

// NewPayrollService — wires up the service with its repository dependencies.
func NewPayrollService(pr repository.PayrollRepository, er repository.EmployeeRepository) *PayrollService {
	return &PayrollService{payrollRepo: pr, employeeRepo: er}
}

// CreatePayroll — fetches active employees, builds the batch, commits atomically, enqueues.
func (s *PayrollService) CreatePayroll(orgID, period string) (*models.Payroll, error) {
	employees, err := s.employeeRepo.FindAllActive(orgID)
	if err != nil {
		return nil, err
	}
	if len(employees) == 0 {
		return nil, fmt.Errorf("no active employees found for this organization")
	}

	payroll := models.Payroll{
		OrganizationID: orgID,
		Period:         period,
		Status:         models.PayrollPending,
	}
	for _, emp := range employees {
		payroll.TotalAmount += emp.Salary
	}

	// Atomic: payroll header + all items commit together or not at all.
	err = models.DB.Transaction(func(tx *gorm.DB) error {
		txPayrollRepo := repository.NewPayrollRepository(tx)
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

	// Enqueue after commit — failed enqueue leaves payroll in "pending" for manual retry.
	payload, _ := json.Marshal(map[string]string{"payroll_id": payroll.ID, "org_id": orgID})
	task := asynq.NewTask(workers.TypeProcessPayroll, payload)
	if _, err := workers.Client.Enqueue(task); err != nil {
		return nil, fmt.Errorf("payroll created but queue rejected it: %v", err)
	}

	return &payroll, nil
}

// GetPayroll — fetches a payroll batch with all its items, scoped to the org.
func (s *PayrollService) GetPayroll(orgID, id string) (*models.Payroll, error) {
	return s.payrollRepo.FindWithItems(orgID, id)
}
