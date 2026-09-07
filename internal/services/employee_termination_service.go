package services

import (
	"context"
	"errors"

	"go-payroll-engine/internal/models"

	"gorm.io/gorm"
)

// EmployeeTerminationService owns what happens to a worker's outstanding EWA
// advances when their employment ends. Employee.IsActive=false already blocks
// new draws (eligibilityTx's DeclineInactiveAccount check) but does nothing
// on its own about advances already in flight — an Approved advance may still
// have a disbursement task queued for it, and a Disbursed one has a
// receivable no future payroll run will ever collect.
type EmployeeTerminationService struct{}

// NewEmployeeTerminationService constructs the service.
func NewEmployeeTerminationService() *EmployeeTerminationService {
	return &EmployeeTerminationService{}
}

// Terminate deactivates the employee and resolves every outstanding advance:
// Approved advances are cancelled (cash never left, same treatment as a
// settlement-time cancellation); Disbursed advances are written off (cash did
// leave, and there is no more payroll to recover it from).
//
// Idempotent by design rather than by an early-return special case: the
// employee update is a no-op if already inactive, the audit entry is only
// written on an actual active→inactive transition, and each advance
// resolution uses the same CAS transition every other part of this codebase
// does — a second call after a partial failure re-resolves only whatever is
// still actually outstanding.
func (s *EmployeeTerminationService) Terminate(
	ctx context.Context, orgID, employeeID, reason, actorIP string,
) (*models.Employee, error) {
	var emp models.Employee
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		if err := tx.First(&emp, "id = ?", employeeID).Error; err != nil {
			return err
		}

		if emp.IsActive {
			if err := tx.Model(&models.Employee{}).Where("id = ?", employeeID).
				Update("is_active", false).Error; err != nil {
				return err
			}
			emp.IsActive = false
			if err := models.AppendAuditTx(tx, orgID, "Employee", employeeID, "terminated",
				"active", reason, actorIP, ""); err != nil {
				return err
			}
		}

		var advances []models.EWAAdvance
		if err := tx.Where("employee_id = ? AND status IN ?", employeeID,
			[]models.AdvanceStatus{models.AdvanceApproved, models.AdvanceDisbursed}).
			Find(&advances).Error; err != nil {
			return err
		}

		for i := range advances {
			adv := &advances[i]
			var resolveErr error
			switch adv.Status {
			case models.AdvanceApproved:
				resolveErr = models.CancelAdvance(tx, adv, models.AdvanceApproved, "employee_terminated")
			case models.AdvanceDisbursed:
				resolveErr = models.WriteOffAdvance(tx, adv, "employee_terminated")
			}
			if resolveErr != nil && !errors.Is(resolveErr, models.ErrStaleStatus) {
				return resolveErr
			}
		}
		return nil
	})
	return &emp, err
}
