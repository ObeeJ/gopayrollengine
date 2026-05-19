package repository

import (
	"fmt"
	"go-payroll-engine/internal/models"

	"gorm.io/gorm"
)

type payrollRepo struct{ db *gorm.DB }

// NewPayrollRepository — hands you a clerk who knows how to talk to the payrolls table.
func NewPayrollRepository(db *gorm.DB) PayrollRepository {
	return &payrollRepo{db: db}
}

// WithTx — returns the repo rebound to tx so queries inherit the caller's RLS scope.
func (r *payrollRepo) WithTx(tx *gorm.DB) PayrollRepository {
	return &payrollRepo{db: tx}
}

func (r *payrollRepo) Create(payroll *models.Payroll) error {
	return r.db.Create(payroll).Error
}

func (r *payrollRepo) CreateItem(item *models.PayrollItem) error {
	return r.db.Create(item).Error
}

func (r *payrollRepo) FindByID(orgID, id string) (*models.Payroll, error) {
	var payroll models.Payroll
	err := r.db.Where("organization_id = ? AND id = ?", orgID, id).First(&payroll).Error
	return &payroll, err
}

func (r *payrollRepo) FindWithItems(orgID, id string) (*models.Payroll, error) {
	var payroll models.Payroll
	err := r.db.Where("organization_id = ? AND id = ?", orgID, id).
		Preload("Items").First(&payroll).Error
	return &payroll, err
}

func (r *payrollRepo) FindCompleted(orgID string, limit int) ([]models.Payroll, error) {
	var payrolls []models.Payroll
	err := r.db.Where("organization_id = ? AND status = ?", orgID, models.PayrollCompleted).
		Order("created_at desc").Limit(limit).Find(&payrolls).Error
	return payrolls, err
}

func (r *payrollRepo) UpdateStatus(payroll *models.Payroll, next models.PayrollStatus) error {
	return models.TransitionStatus(r.db, payroll, payroll.Status, next)
}

// DecrementPendingCount — UPDATE...RETURNING in one round-trip; no TOCTOU, no double reconcile.
func (r *payrollRepo) DecrementPendingCount(payrollID string) (int, error) {
	var newCount int
	err := r.db.Raw(
		`UPDATE payrolls
		    SET pending_count = pending_count - 1,
		        updated_at    = NOW()
		  WHERE id = ?
		RETURNING pending_count`,
		payrollID,
	).Scan(&newCount).Error
	if err != nil {
		return 0, err
	}
	return newCount, nil
}

func (r *payrollRepo) UpdateItemStatus(item *models.PayrollItem, next models.PayrollStatus) error {
	return models.TransitionStatus(r.db, item, item.Status, next)
}

func (r *payrollRepo) FindItemByRef(ref string) (*models.PayrollItem, error) {
	var item models.PayrollItem
	err := r.db.First(&item, "id = ?", ref).Error
	if err != nil {
		return nil, fmt.Errorf("payroll item not found for ref %s: %w", ref, err)
	}
	return &item, nil
}
