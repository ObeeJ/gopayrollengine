-- 000020_accrual_snapshot_wage_type.down.sql

BEGIN;

ALTER TABLE ewa_accrual_snapshots DROP COLUMN IF EXISTS hourly_rate_kobo;
ALTER TABLE ewa_accrual_snapshots DROP COLUMN IF EXISTS wage_type;

COMMIT;
