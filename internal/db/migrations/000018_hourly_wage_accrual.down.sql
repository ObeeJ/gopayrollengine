-- 000018_hourly_wage_accrual.down.sql

BEGIN;

DROP TABLE IF EXISTS time_entries;

ALTER TABLE employees DROP COLUMN IF EXISTS hourly_rate_kobo;
ALTER TABLE employees DROP COLUMN IF EXISTS wage_type;

COMMIT;
