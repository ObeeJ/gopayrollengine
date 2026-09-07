-- 000020_accrual_snapshot_wage_type.up.sql
-- ewa_accrual_snapshots (migration 000014) predates hourly/gig accrual
-- (migration 000018) and was defined with only a fixed monthly salary in
-- mind. Task: populate the table via a daily snapshot job — see
-- internal/services/accrual_snapshot_collector.go. For an hourly employee,
-- salary_kobo alone is meaningless (they don't use that column at all — see
-- Employee.WageType), so a snapshot needs to record which basis it used.

BEGIN;

ALTER TABLE ewa_accrual_snapshots ADD COLUMN IF NOT EXISTS wage_type TEXT NOT NULL DEFAULT 'salaried'
    CHECK (wage_type IN ('salaried', 'hourly'));

ALTER TABLE ewa_accrual_snapshots ADD COLUMN IF NOT EXISTS hourly_rate_kobo BIGINT NOT NULL DEFAULT 0
    CHECK (hourly_rate_kobo >= 0);

COMMIT;
