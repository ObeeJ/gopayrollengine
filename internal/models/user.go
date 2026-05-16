package models

import (
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// User — the worker's app identity; one Employee can have at most one User.
type User struct {
	ID          string         `gorm:"primaryKey" json:"id"`
	EmployeeID  string         `gorm:"uniqueIndex;not null" json:"employee_id"`
	OrgID       string         `gorm:"index;not null" json:"org_id"`
	Phone       string         `gorm:"uniqueIndex;not null" json:"phone"`
	IsActive    bool           `gorm:"default:true" json:"is_active"`
	LastLoginAt *time.Time     `json:"last_login_at,omitempty"`
	CreatedAt   time.Time      `json:"created_at"`
	UpdatedAt   time.Time      `json:"updated_at"`
	DeletedAt   gorm.DeletedAt `gorm:"index" json:"-"`
}

// BeforeCreate — USR- prefix keeps IDs human-readable in logs.
func (u *User) BeforeCreate(tx *gorm.DB) (err error) {
	if u.ID == "" {
		u.ID = "USR-" + uuid.New().String()[:8]
	}
	return
}

// AdvanceRequest — a worker's early wage access request.
type AdvanceRequest struct {
	ID           string         `gorm:"primaryKey" json:"id"`
	OrgID        string         `gorm:"index;not null" json:"org_id"`
	EmployeeID   string         `gorm:"index;not null" json:"employee_id"`
	UserID       string         `gorm:"index;not null" json:"user_id"`
	Amount       float64        `json:"amount"`
	Reason       string         `json:"reason"`
	Status       string         `gorm:"default:pending" json:"status"`
	PaydayTarget time.Time      `json:"payday_target"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
	DeletedAt    gorm.DeletedAt `gorm:"index" json:"-"`
}

// BeforeCreate — ADV- prefix; consistent with the rest of the ID namespace.
func (a *AdvanceRequest) BeforeCreate(tx *gorm.DB) (err error) {
	if a.ID == "" {
		a.ID = "ADV-" + uuid.New().String()[:8]
	}
	return
}
