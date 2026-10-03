package services

import (
	"context"
	"log"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/observability"

	"gorm.io/gorm"
)

// StuckPayoutScanner finds payroll items that have been with the bank longer
// than models.StuckAfter without a result — a lost callback, or a worker that
// died between committing the hand-off and calling the bank. Without it, such
// a payout is only noticed by whoever happens to open that batch.
//
// It runs inside the long-lived API process rather than as a scheduled
// one-shot job, because its output is a Prometheus gauge: a gauge set in a
// process that exits a second later is never scraped.
type StuckPayoutScanner struct {
	interval time.Duration
}

// NewStuckPayoutScanner — interval is how often Run rescans.
func NewStuckPayoutScanner(interval time.Duration) *StuckPayoutScanner {
	return &StuckPayoutScanner{interval: interval}
}

// Scan returns the number of overdue items per organization (only orgs with
// at least one). organizations is the tenant root and has no RLS; payroll_items
// does, so each org is counted inside its own scope — one query per org, which
// is fine until orgs number in the thousands, at which point a SECURITY DEFINER
// aggregate is the next step.
func (s *StuckPayoutScanner) Scan(ctx context.Context) (map[string]int, error) {
	var orgIDs []string
	if err := models.DB.WithContext(ctx).Model(&models.Organization{}).Pluck("id", &orgIDs).Error; err != nil {
		return nil, err
	}
	cutoff := time.Now().Add(-models.StuckAfter)
	out := make(map[string]int)
	for _, orgID := range orgIDs {
		var n int64
		err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
			return tx.Model(&models.PayrollItem{}).
				Where("status = ? AND COALESCE(sent_at, updated_at) < ?", models.PayrollProcessing, cutoff).
				Count(&n).Error
		})
		if err != nil {
			return nil, err
		}
		if n > 0 {
			out[orgID] = int(n)
		}
	}
	return out, nil
}

// Run scans immediately and then every interval until ctx is cancelled,
// setting the payroll_items_stuck gauge and logging CRITICAL per affected org.
func (s *StuckPayoutScanner) Run(ctx context.Context) {
	tick := func() {
		byOrg, err := s.Scan(ctx)
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("stuck payout scan failed: %v", err)
			}
			return
		}
		total := 0
		for orgID, n := range byOrg {
			total += n
			log.Printf("CRITICAL: org %s has %d payroll payment(s) awaiting the bank's result for over %s — "+ //nolint:gosec // orgID is a DB value, n an int, StuckAfter a duration
				"check the provider and resolve them (POST /payrolls/:id/items/:item_id/resolve)", orgID, n, models.StuckAfter)
		}
		observability.PayrollItemsStuck.Set(float64(total))
	}

	tick()
	t := time.NewTicker(s.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			tick()
		}
	}
}
