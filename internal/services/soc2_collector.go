package services

import (
	"context"
	"encoding/json"
	"fmt"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"
	"log"
	"os"
	"time"

	"gorm.io/gorm"
)

// EvidenceCollector — daily SOC 2 evidence snapshots in JSON; auditors don't need DB access.
type EvidenceCollector struct {
	outputDir string
}

// NewEvidenceCollector — reads output path from env; defaults to ./evidence for local runs.
func NewEvidenceCollector() *EvidenceCollector {
	dir := os.Getenv("SOC2_EVIDENCE_DIR")
	if dir == "" {
		dir = "./evidence"
	}
	return &EvidenceCollector{outputDir: dir}
}

// EvidenceSnapshot — one day's worth of auditable facts; the raw material for SOC 2 Type II.
type EvidenceSnapshot struct {
	CollectedAt      time.Time        `json:"collected_at"`
	Period           string           `json:"period"` // "2025-07-01"
	AuditEventCount  int64            `json:"audit_event_count"`
	PayrollBatches   []payrollSummary `json:"payroll_batches"`
	AccessPatterns   []accessPattern  `json:"access_patterns"`
	MigrationVersion migrationVersion `json:"migration_version"`
	SecurityChecks   map[string]bool  `json:"security_checks"`
}

type payrollSummary struct {
	ID          string     `json:"id"`
	OrgID       string     `json:"org_id"`
	Period      string     `json:"period"`
	Status      string     `json:"status"`
	TotalAmount money.Kobo `json:"total_amount"`
	ItemCount   int64      `json:"item_count"`
}

type accessPattern struct {
	ActorKey   string `json:"actor_key"`
	Action     string `json:"action"`
	EntityType string `json:"entity_type"`
	Count      int64  `json:"count"`
	Date       string `json:"date"`
}

type migrationVersion struct {
	Version int64 `json:"version"`
	Dirty   bool  `json:"dirty"`
}

// Collect — writes one day of evidence to a JSON file; back the output dir up to S3.
//
// audit_events, payrolls and payroll_items all have forced row-level security,
// so every read happens inside that org's own WithOrgScope, one org at a
// time. Reading them unscoped (as this used to) returns zero rows under the
// production database role — the evidence file would have reported no audit
// activity and no payroll runs, every day, while tests run as a superuser
// (which bypasses RLS) saw real numbers. Any read error now fails the run:
// silently-empty evidence is worse than none, because it looks like proof.
func (ec *EvidenceCollector) Collect(ctx context.Context, date time.Time) error {
	dateStr := date.Format("2006-01-02")
	startOfDay := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	endOfDay := startOfDay.Add(24 * time.Hour)

	snapshot := EvidenceSnapshot{
		CollectedAt: time.Now().UTC(),
		Period:      dateStr,
	}

	// organizations is the tenant root and carries no RLS, so listing every
	// org is unrestricted.
	var orgs []models.Organization
	if err := models.DB.WithContext(ctx).Select("id").Find(&orgs).Error; err != nil {
		return fmt.Errorf("listing organizations failed: %w", err)
	}

	type result struct {
		ActorKey   string
		Action     string
		EntityType string
		Count      int64
	}

	for _, org := range orgs {
		err := models.WithOrgScope(ctx, org.ID, func(tx *gorm.DB) error {
			// Audit events for the day — proves the audit log is active and growing.
			var auditCount int64
			if err := tx.Model(&models.AuditEvent{}).
				Where("created_at >= ? AND created_at < ?", startOfDay, endOfDay).
				Count(&auditCount).Error; err != nil {
				return err
			}
			snapshot.AuditEventCount += auditCount

			// Payroll batches — proves financial controls are operating.
			var payrolls []models.Payroll
			if err := tx.Where("created_at >= ? AND created_at < ?", startOfDay, endOfDay).
				Find(&payrolls).Error; err != nil {
				return err
			}
			for _, p := range payrolls {
				var itemCount int64
				if err := tx.Model(&models.PayrollItem{}).
					Where("payroll_id = ?", p.ID).Count(&itemCount).Error; err != nil {
					return err
				}
				snapshot.PayrollBatches = append(snapshot.PayrollBatches, payrollSummary{
					ID: p.ID, OrgID: p.OrganizationID, Period: p.Period,
					Status: string(p.Status), TotalAmount: p.TotalAmount, ItemCount: itemCount,
				})
			}

			// Access patterns — proves access is monitored.
			var results []result
			if err := tx.Model(&models.AuditEvent{}).
				Select("actor_key, action, entity_type, count(*) as count").
				Where("created_at >= ? AND created_at < ?", startOfDay, endOfDay).
				Group("actor_key, action, entity_type").
				Scan(&results).Error; err != nil {
				return err
			}
			for _, r := range results {
				snapshot.AccessPatterns = append(snapshot.AccessPatterns, accessPattern{
					ActorKey: r.ActorKey, Action: r.Action,
					EntityType: r.EntityType, Count: r.Count, Date: dateStr,
				})
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("evidence for org %s failed: %w", org.ID, err)
		}
	}

	// Read current migration version from schema_migrations table.
	type schemaMigration struct {
		Version int64
		Dirty   bool
	}
	var sm schemaMigration
	if err := models.DB.WithContext(ctx).
		Raw("SELECT version, dirty FROM schema_migrations ORDER BY version DESC LIMIT 1").
		Scan(&sm).Error; err != nil {
		return fmt.Errorf("reading migration version failed: %w", err)
	}
	snapshot.MigrationVersion = migrationVersion(sm)

	// Security checks — binary pass/fail assertions that auditors can verify.
	snapshot.SecurityChecks = map[string]bool{
		"mock_mode_disabled_in_production": os.Getenv("APP_ENV") != "production" || os.Getenv("MOCK_MODE") != "true",
		"encryption_kek_set":               os.Getenv("ENCRYPTION_KEK") != "",
		"jwt_secret_set":                   os.Getenv("JWT_SECRET") != "",
		"app_api_key_set":                  os.Getenv("APP_API_KEY") != "",
		"migration_schema_clean":           !sm.Dirty,
	}

	// Write to file — one JSON file per day, named by date for easy archival.
	if err := os.MkdirAll(ec.outputDir, 0750); err != nil {
		return fmt.Errorf("evidence dir creation failed: %w", err)
	}
	filePath := fmt.Sprintf("%s/soc2-%s.json", ec.outputDir, dateStr)
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("evidence encode failed: %w", err)
	}
	if err := os.WriteFile(filePath, data, 0600); err != nil {
		return fmt.Errorf("evidence write failed: %w", err)
	}

	log.Printf("SOC 2 evidence collected for %s → %s (%d audit events, %d payrolls)",
		dateStr, filePath, snapshot.AuditEventCount, len(snapshot.PayrollBatches))
	return nil
}
