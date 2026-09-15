-- 000027_payroll_shift_overtime_rules.down.sql
BEGIN;
DROP TABLE IF EXISTS payroll_policies;
ALTER TABLE time_entries DROP COLUMN IF EXISTS shift_type;
COMMIT;
