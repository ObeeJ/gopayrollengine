package repository

import "go-payroll-engine/internal/models"

// EmployeeRepository — the clerk who handles everything employee-shaped in the filing cabinet.
// Services talk to this interface, never to GORM directly.
type EmployeeRepository interface {
	Create(emp *models.Employee) error
	FindByID(orgID, id string) (*models.Employee, error)
	FindAllActive(orgID string) ([]models.Employee, error)
	FindByIDs(orgID string, ids []string) ([]models.Employee, error)
	ListPaginated(orgID string, page, pageSize int) ([]models.Employee, int64, error)
}

// PayrollRepository — the clerk who handles payroll batches and their line items.
type PayrollRepository interface {
	Create(payroll *models.Payroll) error
	CreateItem(item *models.PayrollItem) error
	FindByID(orgID, id string) (*models.Payroll, error)
	FindWithItems(orgID, id string) (*models.Payroll, error)
	FindCompleted(orgID string, limit int) ([]models.Payroll, error)
	UpdateStatus(payroll *models.Payroll, next models.PayrollStatus) error
	DecrementPendingCount(payrollID string) (int, error)
	UpdateItemStatus(item *models.PayrollItem, next models.PayrollStatus) error
	FindItemByRef(ref string) (*models.PayrollItem, error)
}

// OrganizationRepository — the clerk who handles org identity and credentials.
type OrganizationRepository interface {
	FindByID(id string) (*models.Organization, error)
	Create(org *models.Organization) error
}

// UserRepository — the clerk who handles worker (employee-user) identity.
// Workers are the humans who use EWA — distinct from the Employee payroll record.
type UserRepository interface {
	Create(user *models.User) error
	FindByEmployeeID(employeeID string) (*models.User, error)
	FindByPhone(phone string) (*models.User, error)
	UpdateLastLogin(userID string) error
}
