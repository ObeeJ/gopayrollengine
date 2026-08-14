-- 000016_ewa_worker_preferences.down.sql
BEGIN;
DROP TABLE IF EXISTS ewa_worker_preferences;
ALTER TABLE ewa_policies ALTER COLUMN max_accrual_pct SET DEFAULT 50;
COMMIT;
