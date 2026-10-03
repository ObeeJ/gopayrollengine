package models

import (
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
)

// Employer roles a person can hold. "admin" runs payroll; "viewer" reads;
// "compliance" reads the compliance evidence.
const (
	EmployerRoleAdmin      = "admin"
	EmployerRoleViewer     = "viewer"
	EmployerRoleCompliance = "compliance"
)

// ValidEmployerRole reports whether r may be assigned to a person.
func ValidEmployerRole(r string) bool {
	return r == EmployerRoleAdmin || r == EmployerRoleViewer || r == EmployerRoleCompliance
}

// EmployerUser — one person's login to an employer organization.
type EmployerUser struct {
	ID                 string     `gorm:"primaryKey" json:"id"`
	OrganizationID     string     `json:"organization_id"`
	Email              string     `json:"email"`
	Name               string     `json:"name"`
	PasswordHash       string     `json:"-"`
	Role               string     `json:"role"`
	IsActive           bool       `json:"is_active"`
	MustChangePassword bool       `json:"must_change_password"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty"`
	PasswordChangedAt  *time.Time `json:"password_changed_at,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

func (EmployerUser) TableName() string { return "employer_users" }

// BeforeCreate — EUS- prefix keeps IDs readable in audit rows.
func (u *EmployerUser) BeforeCreate(tx *gorm.DB) error {
	if u.ID == "" {
		u.ID = "EUS-" + uuid.New().String()[:8]
	}
	return nil
}

// SetPassword — bcrypt at PasswordCost; never store plaintext.
func (u *EmployerUser) SetPassword(plain string) error {
	h, err := bcrypt.GenerateFromPassword([]byte(plain), PasswordCost)
	if err != nil {
		return err
	}
	u.PasswordHash = string(h)
	return nil
}
