-- 000028_sweep_late_approved_entries.down.sql
BEGIN;
DROP INDEX IF EXISTS idx_time_entries_unpaid_approved;
ALTER TABLE time_entries DROP COLUMN IF EXISTS paid_payroll_item_id;
COMMIT;
