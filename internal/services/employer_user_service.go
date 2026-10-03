package services

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/validate"
	"go-payroll-engine/pkg/money"

	"golang.org/x/crypto/bcrypt"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrEmployerUserExists   = errors.New("a user with that email already exists")
	ErrEmployerUserNotFound = errors.New("user not found")
	ErrInvalidEmployerUser  = errors.New("invalid user")
	ErrLastAdmin            = errors.New("an organization must keep at least one active admin")
	ErrWrongPassword        = errors.New("current password is incorrect")
)

// dummyUserHash equalises bcrypt cost for unknown emails (see AuthHandler).
var dummyUserHash = func() []byte {
	h, err := bcrypt.GenerateFromPassword([]byte("timing-equaliser"), models.PasswordCost)
	if err != nil {
		panic("employer users: dummy hash: " + err.Error())
	}
	return h
}()

// EmployerUserService manages the people who can log in to an employer org.
type EmployerUserService struct{}

func NewEmployerUserService() *EmployerUserService { return &EmployerUserService{} }

// TempPassword returns a random one-time password (shown once, must be changed).
func TempPassword() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil // 24 chars
}

// Authenticate verifies email+password. It returns ErrEmployerUserNotFound for
// every failure (unknown email, wrong password, deactivated person or org) so
// callers cannot tell them apart; bcrypt always runs.
func (s *EmployerUserService) Authenticate(ctx context.Context, email, password string) (*models.EmployerUser, error) {
	var u models.EmployerUser
	res := models.DB.WithContext(ctx).Raw("SELECT * FROM find_employer_user_for_login(?)", strings.TrimSpace(email)).Scan(&u)
	if res.Error != nil {
		return nil, res.Error
	}
	hash := dummyUserHash
	if res.RowsAffected > 0 {
		hash = []byte(u.PasswordHash)
	}
	pwErr := bcrypt.CompareHashAndPassword(hash, []byte(password))
	if res.RowsAffected == 0 || pwErr != nil || !u.IsActive {
		return nil, ErrEmployerUserNotFound
	}
	var org models.Organization
	if err := models.DB.WithContext(ctx).Select("id, is_active, is_d2c").First(&org, "id = ?", u.OrganizationID).Error; err != nil {
		return nil, ErrEmployerUserNotFound
	}
	if !org.IsActive || org.IsD2C {
		return nil, ErrEmployerUserNotFound
	}
	now := time.Now()
	_ = models.WithOrgScope(ctx, u.OrganizationID, func(tx *gorm.DB) error {
		return tx.Model(&models.EmployerUser{}).Where("id = ?", u.ID).UpdateColumn("last_login_at", now).Error
	})
	return &u, nil
}

// IsActive — for token revalidation; a missing person counts as inactive.
func (s *EmployerUserService) IsActive(orgID, userID string) (bool, error) {
	var active bool
	err := models.WithOrgScope(context.Background(), orgID, func(tx *gorm.DB) error {
		var u models.EmployerUser
		if err := tx.Select("is_active").First(&u, "id = ? AND organization_id = ?", userID, orgID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		active = u.IsActive
		return nil
	})
	return active, err
}

func normalizeEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

// Create adds a person with a generated temporary password (returned once).
func (s *EmployerUserService) Create(ctx context.Context, orgID, email, name, role string, actor Actor) (*models.EmployerUser, string, error) {
	email = normalizeEmail(email)
	if !validate.Email(email) {
		return nil, "", fmt.Errorf("%w: email is not valid", ErrInvalidEmployerUser)
	}
	if !models.ValidEmployerRole(role) {
		return nil, "", fmt.Errorf("%w: role must be admin, viewer or compliance", ErrInvalidEmployerUser)
	}
	temp, err := TempPassword()
	if err != nil {
		return nil, "", err
	}
	u := &models.EmployerUser{OrganizationID: orgID, Email: email, Name: strings.TrimSpace(name),
		Role: role, IsActive: true, MustChangePassword: true}
	if err := u.SetPassword(temp); err != nil {
		return nil, "", err
	}
	err = models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		if err := tx.Create(u).Error; err != nil {
			return err
		}
		return models.AppendAuditTx(tx, orgID, "EmployerUser", u.ID, "user_created", "", role, actor.IP, actor.Name)
	})
	if err != nil {
		if strings.Contains(err.Error(), "uq_employer_users_email") || strings.Contains(err.Error(), "23505") {
			return nil, "", ErrEmployerUserExists
		}
		return nil, "", err
	}
	return u, temp, nil
}

// List returns the org's people, oldest first.
func (s *EmployerUserService) List(ctx context.Context, orgID string) ([]models.EmployerUser, error) {
	var out []models.EmployerUser
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		return tx.Where("organization_id = ?", orgID).Order("created_at ASC, id ASC").Find(&out).Error
	})
	return out, err
}

// EmployerUserUpdate — nil fields are left unchanged.
type EmployerUserUpdate struct {
	Role     *string
	IsActive *bool
}

// Update changes a person's role or active flag, never leaving the org
// without an active admin.
func (s *EmployerUserService) Update(ctx context.Context, orgID, userID string, upd EmployerUserUpdate, actor Actor) (*models.EmployerUser, error) {
	if upd.Role == nil && upd.IsActive == nil {
		return nil, fmt.Errorf("%w: nothing to update", ErrInvalidEmployerUser)
	}
	if upd.Role != nil && !models.ValidEmployerRole(*upd.Role) {
		return nil, fmt.Errorf("%w: role must be admin, viewer or compliance", ErrInvalidEmployerUser)
	}
	var u models.EmployerUser
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		// Serialise admin-set changes per org so two concurrent demotions
		// cannot each see "another admin remains".
		if err := tx.Exec("SELECT pg_advisory_xact_lock(hashtextextended(?, 0))", "employer_users:"+orgID).Error; err != nil {
			return err
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&u, "id = ? AND organization_id = ?", userID, orgID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrEmployerUserNotFound
			}
			return err
		}
		before := fmt.Sprintf("%s/active=%t", u.Role, u.IsActive)
		newRole, newActive := u.Role, u.IsActive
		if upd.Role != nil {
			newRole = *upd.Role
		}
		if upd.IsActive != nil {
			newActive = *upd.IsActive
		}
		if u.Role == models.EmployerRoleAdmin && u.IsActive && (newRole != models.EmployerRoleAdmin || !newActive) {
			var others int64
			if err := tx.Model(&models.EmployerUser{}).
				Where("organization_id = ? AND role = ? AND is_active AND id <> ?", orgID, models.EmployerRoleAdmin, u.ID).Count(&others).Error; err != nil {
				return err
			}
			if others == 0 {
				return ErrLastAdmin
			}
		}
		if err := tx.Model(&u).Updates(map[string]interface{}{"role": newRole, "is_active": newActive, "updated_at": time.Now()}).Error; err != nil {
			return err
		}
		u.Role, u.IsActive = newRole, newActive
		return models.AppendAuditTx(tx, orgID, "EmployerUser", u.ID, "user_updated", before,
			fmt.Sprintf("%s/active=%t", newRole, newActive), actor.IP, actor.Name)
	})
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// ResetPassword issues a new temporary password (returned once).
func (s *EmployerUserService) ResetPassword(ctx context.Context, orgID, userID string, actor Actor) (string, error) {
	temp, err := TempPassword()
	if err != nil {
		return "", err
	}
	var hashHolder models.EmployerUser
	if err := hashHolder.SetPassword(temp); err != nil {
		return "", err
	}
	err = models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		res := tx.Model(&models.EmployerUser{}).Where("id = ? AND organization_id = ?", userID, orgID).Updates(map[string]interface{}{
			"password_hash": hashHolder.PasswordHash, "must_change_password": true, "updated_at": time.Now()})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrEmployerUserNotFound
		}
		return models.AppendAuditTx(tx, orgID, "EmployerUser", userID, "password_reset", "", "", actor.IP, actor.Name)
	})
	if err != nil {
		return "", err
	}
	return temp, nil
}

// ChangePassword lets a person set their own password (clears the temporary flag).
func (s *EmployerUserService) ChangePassword(ctx context.Context, orgID, userID, current, next, ip string) error {
	if msg := validate.Password(next); msg != "" {
		return fmt.Errorf("%w: %s", ErrInvalidEmployerUser, msg)
	}
	if current == next {
		return fmt.Errorf("%w: new password must differ from the current one", ErrInvalidEmployerUser)
	}
	return models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		var u models.EmployerUser
		if err := tx.First(&u, "id = ? AND organization_id = ?", userID, orgID).Error; err != nil {
			return ErrEmployerUserNotFound
		}
		if bcrypt.CompareHashAndPassword([]byte(u.PasswordHash), []byte(current)) != nil {
			return ErrWrongPassword
		}
		if err := u.SetPassword(next); err != nil {
			return err
		}
		now := time.Now()
		if err := tx.Model(&u).Updates(map[string]interface{}{
			"password_hash": u.PasswordHash, "must_change_password": false,
			"password_changed_at": now, "updated_at": now}).Error; err != nil {
			return err
		}
		return models.AppendAuditTx(tx, orgID, "EmployerUser", userID, "password_changed", "", "", ip, userID)
	})
}

// CreateOrganization onboards an employer: the org plus its first admin, with
// no org-level password (so the shared-password login stays disabled).
func (s *EmployerUserService) CreateOrganization(ctx context.Context, name string, currency money.Currency, adminEmail string) (*models.Organization, *models.EmployerUser, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, nil, "", fmt.Errorf("%w: organization name is required", ErrInvalidEmployerUser)
	}
	if !currency.IsValid() {
		return nil, nil, "", fmt.Errorf("%w: unsupported currency", ErrInvalidEmployerUser)
	}
	org := &models.Organization{Name: name, Currency: currency, Role: models.EmployerRoleAdmin}
	if err := models.DB.WithContext(ctx).Create(org).Error; err != nil {
		return nil, nil, "", err
	}
	u, temp, err := s.Create(ctx, org.ID, adminEmail, "", models.EmployerRoleAdmin, Actor{Name: "onboarding-cli"})
	if err != nil {
		// Do not leave an org nobody can log in to.
		_ = models.DB.WithContext(ctx).Delete(&models.Organization{}, "id = ?", org.ID).Error
		return nil, nil, "", err
	}
	return org, u, temp, nil
}
