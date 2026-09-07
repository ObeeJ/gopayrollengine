package services

import (
	"context"
	"log"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AccrualSnapshotCollector populates ewa_accrual_snapshots (migration
// 000014) — a dated record of what each active employee had earned, kept so
// an EWA eligibility decision can be reconstructed months later during a
// dispute, without having to trust that AccruedToDate would recompute the
// same answer against data that may have since changed. Meant to run once
// daily via APP_MODE=snapshot-accruals (cmd/api/main.go), the same
// external-cron pattern EvidenceCollector already uses for SOC 2 evidence.
type AccrualSnapshotCollector struct {
	ewa *EWAService
}

// NewAccrualSnapshotCollector constructs the collector.
func NewAccrualSnapshotCollector() *AccrualSnapshotCollector {
	return &AccrualSnapshotCollector{ewa: NewEWAService()}
}

// Collect snapshots every active employee's accrued-to-date as of asOf.
//
// organizations has no RLS (it is the tenant root — everything else scopes
// off it), so listing every org is unrestricted; each org's employees are
// then read and written inside its own WithOrgScope transaction, same as any
// other tenant-scoped access in this codebase. One org's error is logged and
// skipped rather than aborting the whole run — a bad row in one tenant must
// not cost every other tenant their day's snapshot.
func (c *AccrualSnapshotCollector) Collect(ctx context.Context, asOf time.Time) error {
	// Organization.IsActive has no backing column — no migration has ever
	// created one, so nothing in this codebase can filter by it (a
	// pre-existing model/schema drift, not something to paper over here).
	// Every org is snapshotted; per-employee IsActive below is real and does
	// the filtering that matters for this job.
	var orgs []models.Organization
	if err := models.DB.Find(&orgs).Error; err != nil {
		return err
	}

	asOfDate := time.Date(asOf.Year(), asOf.Month(), asOf.Day(), 0, 0, 0, 0, asOf.Location())
	period := asOf.Format(PeriodLayout)

	for _, org := range orgs {
		if err := c.collectOrg(ctx, org.ID, period, asOfDate, asOf); err != nil {
			log.Printf("accrual snapshot: org %s failed: %v", org.ID, err)
		}
	}
	return nil
}

func (c *AccrualSnapshotCollector) collectOrg(ctx context.Context, orgID, period string, asOfDate, asOf time.Time) error {
	return models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		var employees []models.Employee
		if err := tx.Where("is_active = ?", true).Find(&employees).Error; err != nil {
			return err
		}

		for _, emp := range employees {
			var accrued money.Kobo
			var err error
			if emp.IsHourly() {
				accrued, err = c.ewa.accruedHourlyToDateTx(tx, orgID, emp.ID, emp.HourlyRateKobo, period, asOf)
			} else {
				accrued, err = AccruedToDate(emp.Salary, period, asOf)
			}
			if err != nil {
				log.Printf("accrual snapshot: %s/%s failed: %v", orgID, emp.ID, err)
				continue
			}

			snapshot := models.EWAAccrualSnapshot{
				OrganizationID: orgID,
				EmployeeID:     emp.ID,
				Period:         period,
				AsOfDate:       asOfDate,
				AccruedKobo:    accrued,
				SalaryKobo:     emp.Salary,
				WageType:       emp.WageType,
				HourlyRateKobo: emp.HourlyRateKobo,
			}
			// Upsert on (org, employee, date) — migration 000014's unique
			// constraint — so re-running for the same day corrects that
			// day's row instead of piling up duplicates.
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{{Name: "organization_id"}, {Name: "employee_id"}, {Name: "as_of_date"}},
				DoUpdates: clause.AssignmentColumns([]string{
					"period", "accrued_kobo", "salary_kobo", "wage_type", "hourly_rate_kobo",
				}),
			}).Create(&snapshot).Error; err != nil {
				log.Printf("accrual snapshot: %s/%s write failed: %v", orgID, emp.ID, err)
			}
		}
		return nil
	})
}
