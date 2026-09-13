package services

import (
	"context"
	"fmt"
	"math"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"

	"gorm.io/gorm"
)

// MinKAnonymity is the smallest group size the dashboard will ever report a
// figure for. Below it, even an aggregate breakdown risks re-identifying a
// specific worker in a small team — the roadmap calls this a hard,
// non-negotiable constraint: employers see aggregates, never individuals.
const MinKAnonymity = 10

// EmployerDashboardService produces aggregate, anonymised dependency-tier
// reporting for employers — the product's answer to "no visibility into
// workforce financial stress until someone resigns," without ever exposing a
// per-worker signal that could be used as a firing or retaliation vector.
type EmployerDashboardService struct {
	ewa     *EWAService
	empRepo repository.EmployeeRepository
}

// NewEmployerDashboardService wires up the service with its dependencies.
func NewEmployerDashboardService(empRepo repository.EmployeeRepository) *EmployerDashboardService {
	return &EmployerDashboardService{ewa: NewEWAService(), empRepo: empRepo}
}

// TierCount is one bucket of the breakdown. Count and Percent are nil
// whenever the bucket itself has fewer than MinKAnonymity employees in it —
// omitted from the JSON response entirely rather than shown as a small,
// individually identifying number. A true zero is not suppressed: "0 people
// are Dependent" carries no re-identification risk and is real signal.
type TierCount struct {
	Tier       models.DependencyTier `json:"tier"`
	Count      *int                  `json:"count,omitempty"`
	Percent    *float64              `json:"percent,omitempty"`
	Suppressed bool                  `json:"suppressed"`
}

// WorkforceDependencyReport is the full aggregate response.
type WorkforceDependencyReport struct {
	OrganizationID  string    `json:"organization_id"`
	AsOf            time.Time `json:"as_of"`
	ActiveEmployees int       `json:"active_employees"`
	// Suppressed is set when the ORG ITSELF is below MinKAnonymity — too few
	// employees for any breakdown, however coarse, to be safe to report.
	Suppressed       bool        `json:"suppressed"`
	SuppressedReason string      `json:"suppressed_reason,omitempty"`
	Breakdown        []TierCount `json:"breakdown,omitempty"`
}

var allDependencyTiers = []models.DependencyTier{
	models.TierHealthy, models.TierElevated, models.TierStrained, models.TierDependent,
}

// GetWorkforceDependencyReport computes the current dependency-tier
// breakdown across every active employee in the org, as of asOf.
func (s *EmployerDashboardService) GetWorkforceDependencyReport(
	ctx context.Context, orgID string, asOf time.Time,
) (*WorkforceDependencyReport, error) {
	var report *WorkforceDependencyReport
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		employees, err := s.empRepo.WithTx(tx).FindAllActive(orgID)
		if err != nil {
			return err
		}

		report = &WorkforceDependencyReport{
			OrganizationID:  orgID,
			AsOf:            asOf,
			ActiveEmployees: len(employees),
		}
		if len(employees) < MinKAnonymity {
			report.Suppressed = true
			report.SuppressedReason = fmt.Sprintf(
				"fewer than %d active employees; aggregate reporting is suppressed to protect individual privacy",
				MinKAnonymity)
			return nil
		}

		counts := make(map[models.DependencyTier]int, len(allDependencyTiers))
		for _, emp := range employees {
			tier, err := s.ewa.currentDependencyTierTx(tx, orgID, emp.ID, &emp, asOf)
			if err != nil {
				return fmt.Errorf("dependency assessment for %s failed: %w", emp.ID, err)
			}
			counts[tier]++
		}

		total := len(employees)
		report.Breakdown = buildBreakdown(counts, total)
		return nil
	})
	return report, err
}

// buildBreakdown applies k-anonymity suppression to the four tier counts.
//
// Primary suppression hides any bucket whose own count is small (nonzero but
// under MinKAnonymity) — showing "1 person is Dependent" identifies that
// person as surely as naming them.
//
// That alone is not enough: the org's total employee count is always known
// (it's ActiveEmployees, on the report itself), so if exactly one bucket
// ends up hidden, its value is fully recoverable by subtracting every other
// bucket — including exact zeros, which contribute nothing to the sum but
// still narrow it — from the total. Complementary suppression closes that
// gap: when primary suppression leaves exactly one bucket hidden, a second,
// otherwise-safe-to-disclose bucket (the smallest, possibly a real zero) is
// hidden too, so an observer can only learn the pair's combined total, never
// either value alone.
func buildBreakdown(counts map[models.DependencyTier]int, total int) []TierCount {
	hidden := make(map[models.DependencyTier]bool, len(allDependencyTiers))
	hiddenCount := 0
	for _, tier := range allDependencyTiers {
		if c := counts[tier]; c > 0 && c < MinKAnonymity {
			hidden[tier] = true
			hiddenCount++
		}
	}

	if hiddenCount == 1 {
		secondary := models.DependencyTier("")
		secondaryCount := -1
		for _, tier := range allDependencyTiers {
			if hidden[tier] {
				continue
			}
			if secondaryCount == -1 || counts[tier] < secondaryCount {
				secondary, secondaryCount = tier, counts[tier]
			}
		}
		if secondary != "" {
			hidden[secondary] = true
		}
	}

	breakdown := make([]TierCount, 0, len(allDependencyTiers))
	for _, tier := range allDependencyTiers {
		bucket := TierCount{Tier: tier}
		if hidden[tier] {
			bucket.Suppressed = true
		} else {
			c := counts[tier]
			p := percentOf(c, total)
			bucket.Count = &c
			bucket.Percent = &p
		}
		breakdown = append(breakdown, bucket)
	}
	return breakdown
}

// percentOf rounds to one decimal place — enough precision to be useful,
// not enough to make a borderline bucket feel more exact than it is.
func percentOf(count, total int) float64 {
	if total == 0 {
		return 0
	}
	return math.Round(float64(count)/float64(total)*1000) / 10
}
