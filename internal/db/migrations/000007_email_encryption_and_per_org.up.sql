-- 000007_email_encryption_and_per_org.up.sql
-- Closes two related tenant-isolation and PII gaps:
--
--   1. The original schema declared a GLOBAL unique index on employees(email).
--      That meant tenant A could enumerate tenant B's employee emails by
--      probing the unique-violation error on create. Email uniqueness is a
--      per-organisation invariant, not a global one.
--
--   2. The email column was plaintext, while account_number and bank_code
--      were AES-GCM encrypted. Email is PII under NDPR/GDPR and must be
--      encrypted at rest too. Application-side encryption with a random GCM
--      nonce makes equal plaintexts produce different ciphertexts, which
--      breaks any unique constraint on the ciphertext column.
--
-- The fix is the standard searchable-encryption pattern:
--   - email column becomes ciphertext (random-nonce AES-GCM).
--   - email_hmac column stores HMAC-SHA256(plaintext, key) — deterministic
--     and uniquely indexable, with no plaintext recoverable from the digest.
--   - Unique index is (organization_id, email_hmac) — uniqueness is scoped
--     to the tenant, and the digest is the only column that admits equality
--     comparison on encrypted values.
--
-- This migration is safe for a pre-launch repo (no existing employee rows
-- need rewriting). For a live cutover the email_hmac and re-encrypted email
-- would have to be backfilled in a separate batch job before the index
-- swap; that path is out of scope here.

BEGIN;

DROP INDEX IF EXISTS idx_employees_email;

ALTER TABLE employees
    ADD COLUMN IF NOT EXISTS email_hmac BYTEA;

CREATE UNIQUE INDEX IF NOT EXISTS idx_employees_org_email_hmac
    ON employees (organization_id, email_hmac)
    WHERE deleted_at IS NULL AND email_hmac IS NOT NULL;

COMMIT;
