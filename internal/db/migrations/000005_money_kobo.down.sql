-- 000005_money_kobo.down.sql
-- Revert monetary columns from BIGINT (Kobo) back to NUMERIC(20,2) (Naira).
-- Lossless: integer kobo / 100 → exact two-decimal naira.

BEGIN;

ALTER TABLE advance_requests
    DROP CONSTRAINT IF EXISTS advance_requests_amount_nonneg;
ALTER TABLE advance_requests
    ALTER COLUMN amount TYPE NUMERIC(20,2) USING (amount::numeric / 100),
    ALTER COLUMN amount SET DEFAULT 0;

ALTER TABLE payroll_items
    DROP CONSTRAINT IF EXISTS payroll_items_amount_nonneg;
ALTER TABLE payroll_items
    ALTER COLUMN amount TYPE NUMERIC(20,2) USING (amount::numeric / 100),
    ALTER COLUMN amount SET DEFAULT 0;

ALTER TABLE payrolls
    DROP CONSTRAINT IF EXISTS payrolls_total_amount_nonneg;
ALTER TABLE payrolls
    ALTER COLUMN total_amount TYPE NUMERIC(20,2) USING (total_amount::numeric / 100),
    ALTER COLUMN total_amount SET DEFAULT 0;

ALTER TABLE employees
    DROP CONSTRAINT IF EXISTS employees_salary_nonneg;
ALTER TABLE employees
    ALTER COLUMN salary TYPE NUMERIC(20,2) USING (salary::numeric / 100),
    ALTER COLUMN salary SET DEFAULT 0;

COMMIT;
