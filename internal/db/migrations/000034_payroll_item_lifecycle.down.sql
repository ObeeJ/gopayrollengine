BEGIN;
DROP INDEX IF EXISTS idx_payroll_items_employee_recent;
DROP INDEX IF EXISTS idx_payroll_items_processing;
ALTER TABLE payroll_items
    DROP CONSTRAINT IF EXISTS payroll_items_breakdown_nonnegative,
    DROP CONSTRAINT IF EXISTS payroll_items_attempt_positive;
ALTER TABLE payroll_items
    DROP COLUMN IF EXISTS resolution_evidence,
    DROP COLUMN IF EXISTS resolution_note,
    DROP COLUMN IF EXISTS resolved_by,
    DROP COLUMN IF EXISTS settled_at,
    DROP COLUMN IF EXISTS sent_at,
    DROP COLUMN IF EXISTS savings_kobo,
    DROP COLUMN IF EXISTS advances_deducted_kobo,
    DROP COLUMN IF EXISTS gross_kobo,
    DROP COLUMN IF EXISTS attempt;
COMMIT;
