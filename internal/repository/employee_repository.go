package repository

import (
	"go-payroll-engine/internal/models"

	"gorm.io/gorm"
)

type employeeRepo struct{ db *gorm.DB }

// NewEmployeeRepository — hands you a clerk who knows how to talk to the employees table.
func NewEmployeeRepository(db *gorm.DB) EmployeeRepository {
	return &employeeRepo{db: db}
}

func (r *employeeRepo) Create(emp *models.Employee) error {
	return r.db.Create(emp).Error
}

func (r *employeeRepo) FindByID(orgID, id string) (*models.Employee, error) {
	var emp models.Employee
	err := r.db.Where("organization_id = ? AND id = ?", orgID, id).First(&emp).Error
	return &emp, err
}

func (r *employeeRepo) FindAllActive(orgID string) ([]models.Employee, error) {
	var employees []models.Employee
	err := r.db.Where("organization_id = ? AND is_active = ?", orgID, true).Find(&employees).Error
	return employees, err
}

func (r *employeeRepo) FindByIDs(orgID string, ids []string) ([]models.Employee, error) {
	var employees []models.Employee
	err := r.db.Where("organization_id = ? AND id IN ?", orgID, ids).Find(&employees).Error
	return employees, err
}

func (r *employeeRepo) ListPaginated(orgID string, page, pageSize int) ([]models.Employee, int64, error) {
	var employees []models.Employee
	var total int64
	base := r.db.Model(&models.Employee{}).Where("organization_id = ?", orgID)
	if err := base.Count(&total).Error; err != nil {
		return nil, 0, err
	}
	err := base.Offset((page - 1) * pageSize).Limit(pageSize).Find(&employees).Error
	return employees, total, err
}
