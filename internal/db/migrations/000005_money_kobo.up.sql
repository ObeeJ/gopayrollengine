-- 000005_money_kobo.up.sql
-- Migrate all monetary columns from NUMERIC(20,2) (decimal Naira) to BIGINT (Kobo,
-- 1/100 of a Naira). NUMERIC(20,2) silently truncates at the GORM/float64 boundary
-- on every read and write — every value was being rounded twice per round-trip.
-- BIGINT exactly represents ±92 quadrillion kobo (±₦920 trillion) — comfortable.
--
-- The USING clause multiplies by 100 to convert ₦x.yy → integer kobo, with
-- ROUND() guarding against any historical sub-kobo dust introduced by previous
-- float arithmetic. NOT NULL DEFAULT 0 and CHECK (>= 0) are added as structural
-- guarantees that disbursement amounts cannot be negative.

BEGIN;

ALTER TABLE employees
    ALTER COLUMN salary TYPE BIGINT USING ROUND(salary * 100)::BIGINT,
    ALTER COLUMN salary SET DEFAULT 0;
ALTER TABLE employees
    ADD CONSTRAINT employees_salary_nonneg CHECK (salary >= 0);

ALTER TABLE payrolls
    ALTER COLUMN total_amount TYPE BIGINT USING ROUND(total_amount * 100)::BIGINT,
    ALTER COLUMN total_amount SET DEFAULT 0;
ALTER TABLE payrolls
    ADD CONSTRAINT payrolls_total_amount_nonneg CHECK (total_amount >= 0);

ALTER TABLE payroll_items
    ALTER COLUMN amount TYPE BIGINT USING ROUND(amount * 100)::BIGINT,
    ALTER COLUMN amount SET DEFAULT 0;
ALTER TABLE payroll_items
    ADD CONSTRAINT payroll_items_amount_nonneg CHECK (amount >= 0);

ALTER TABLE advance_requests
    ALTER COLUMN amount TYPE BIGINT USING ROUND(amount * 100)::BIGINT,
    ALTER COLUMN amount SET DEFAULT 0;
ALTER TABLE advance_requests
    ADD CONSTRAINT advance_requests_amount_nonneg CHECK (amount >= 0);

COMMIT;
