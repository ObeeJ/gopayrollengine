-- 000007_email_encryption_and_per_org.down.sql
-- Reverts the per-org email index and drops the blind-index column.
-- WARNING: existing employee rows will retain ciphertext in the email column
-- until application code is also rolled back. The global unique index is
-- restored as a partial index excluding deleted rows.

BEGIN;

DROP INDEX IF EXISTS idx_employees_org_email_hmac;

ALTER TABLE employees DROP COLUMN IF EXISTS email_hmac;

CREATE UNIQUE INDEX IF NOT EXISTS idx_employees_email
    ON employees (email)
    WHERE deleted_at IS NULL;

COMMIT;
