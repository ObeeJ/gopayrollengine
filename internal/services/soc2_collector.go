package services

import (
	"encoding/json"
	"fmt"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"
	"log"
	"os"
	"time"
)

// EvidenceCollector — automated SOC 2 evidence snapshots; runs daily via cron or a scheduled task.
// Produces JSON evidence files that auditors can verify without DB access.
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
	CollectedAt      time.Time              `json:"collected_at"`
	Period           string                 `json:"period"` // "2025-07-01"
	AuditEventCount  int64                  `json:"audit_event_count"`
	PayrollBatches   []payrollSummary       `json:"payroll_batches"`
	AccessPatterns   []accessPattern        `json:"access_patterns"`
	MigrationVersion migrationVersion       `json:"migration_version"`
	SecurityChecks   map[string]bool        `json:"security_checks"`
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
	Version int64  `json:"version"`
	Dirty   bool   `json:"dirty"`
}

// Collect — gathers evidence for the given date and writes it to a JSON file.
// Call this from a daily cron job; the output directory should be backed up to S3.
func (ec *EvidenceCollector) Collect(date time.Time) error {
	dateStr := date.Format("2006-01-02")
	startOfDay := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	endOfDay := startOfDay.Add(24 * time.Hour)

	snapshot := EvidenceSnapshot{
		CollectedAt: time.Now().UTC(),
		Period:      dateStr,
	}

	// Count audit events for the day — proves the audit log is active and growing.
	models.DB.Model(&models.AuditEvent{}).
		Where("created_at >= ? AND created_at < ?", startOfDay, endOfDay).
		Count(&snapshot.AuditEventCount)

	// Summarise payroll batches — proves financial controls are operating.
	var payrolls []models.Payroll
	models.DB.Where("created_at >= ? AND created_at < ?", startOfDay, endOfDay).Find(&payrolls)
	for _, p := range payrolls {
		var itemCount int64
		models.DB.Model(&models.PayrollItem{}).Where("payroll_id = ?", p.ID).Count(&itemCount)
		snapshot.PayrollBatches = append(snapshot.PayrollBatches, payrollSummary{
			ID: p.ID, OrgID: p.OrganizationID, Period: p.Period,
			Status: string(p.Status), TotalAmount: p.TotalAmount, ItemCount: itemCount,
		})
	}

	// Aggregate access patterns from audit log — proves access is monitored.
	type result struct {
		ActorKey   string
		Action     string
		EntityType string
		Count      int64
	}
	var results []result
	models.DB.Model(&models.AuditEvent{}).
		Select("actor_key, action, entity_type, count(*) as count").
		Where("created_at >= ? AND created_at < ?", startOfDay, endOfDay).
		Group("actor_key, action, entity_type").
		Scan(&results)
	for _, r := range results {
		snapshot.AccessPatterns = append(snapshot.AccessPatterns, accessPattern{
			ActorKey: r.ActorKey, Action: r.Action,
			EntityType: r.EntityType, Count: r.Count, Date: dateStr,
		})
	}

	// Read current migration version from schema_migrations table.
	type schemaMigration struct {
		Version int64
		Dirty   bool
	}
	var sm schemaMigration
	models.DB.Raw("SELECT version, dirty FROM schema_migrations ORDER BY version DESC LIMIT 1").Scan(&sm)
	snapshot.MigrationVersion = migrationVersion{Version: sm.Version, Dirty: sm.Dirty}

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
	data, _ := json.MarshalIndent(snapshot, "", "  ")
	if err := os.WriteFile(filePath, data, 0640); err != nil {
		return fmt.Errorf("evidence write failed: %w", err)
	}

	log.Printf("SOC 2 evidence collected for %s → %s (%d audit events, %d payrolls)",
		dateStr, filePath, snapshot.AuditEventCount, len(snapshot.PayrollBatches))
	return nil
}
