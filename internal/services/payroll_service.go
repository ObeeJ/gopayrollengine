package services

import (
	"encoding/json"
	"fmt"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/workers"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

type PayrollService struct{}

// CreatePayroll — fetches active employees, builds the batch, and hands it to the worker queue.
// The DB transaction and Redis enqueue are intentionally separate: commit first, enqueue second.
func (s *PayrollService) CreatePayroll(orgID, period string) (*models.Payroll, error) {
	var employees []models.Employee
	// Scoped to org — one tenant's employees never bleed into another's payroll.
	if err := models.ScopedDB(orgID).Where("is_active = ?", true).Find(&employees).Error; err != nil {
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

	// Atomic: both the payroll header and all items commit together or not at all.
	err := models.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&payroll).Error; err != nil {
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
			if err := tx.Create(&item).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	// Enqueue after commit — a failed enqueue leaves the payroll in "pending" for manual retry.
	payload, _ := json.Marshal(map[string]string{"payroll_id": payroll.ID, "org_id": orgID})
	task := asynq.NewTask(workers.TypeProcessPayroll, payload)
	if _, err := workers.Client.Enqueue(task); err != nil {
		return nil, fmt.Errorf("payroll created but queue rejected it: %v", err)
	}

	return &payroll, nil
}
