package repository

import (
	"go-payroll-engine/internal/models"
	"time"

	"gorm.io/gorm"
)

// --- Organization ---

type orgRepo struct{ db *gorm.DB }

// NewOrganizationRepository — the clerk who handles org credentials and identity.
func NewOrganizationRepository(db *gorm.DB) OrganizationRepository {
	return &orgRepo{db: db}
}

func (r *orgRepo) FindByID(id string) (*models.Organization, error) {
	var org models.Organization
	err := r.db.First(&org, "id = ?", id).Error
	return &org, err
}

func (r *orgRepo) Create(org *models.Organization) error {
	return r.db.Create(org).Error
}

// --- User (Worker identity for EWA) ---

type userRepo struct{ db *gorm.DB }

// NewUserRepository — the clerk who handles worker login identity.
func NewUserRepository(db *gorm.DB) UserRepository {
	return &userRepo{db: db}
}

func (r *userRepo) Create(user *models.User) error {
	return r.db.Create(user).Error
}

func (r *userRepo) FindByEmployeeID(employeeID string) (*models.User, error) {
	var user models.User
	err := r.db.First(&user, "employee_id = ?", employeeID).Error
	return &user, err
}

func (r *userRepo) FindByPhone(phone string) (*models.User, error) {
	var user models.User
	err := r.db.First(&user, "phone = ?", phone).Error
	return &user, err
}

func (r *userRepo) UpdateLastLogin(userID string) error {
	return r.db.Model(&models.User{}).Where("id = ?", userID).
		UpdateColumn("last_login_at", time.Now()).Error
}
