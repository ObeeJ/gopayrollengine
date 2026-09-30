-- 000033_hardship_grant_disbursement.up.sql
-- Hardship grants were booked but never paid.
--
-- IssueHardshipGrant posted Dr hardship_grant_expense / Cr cash_settlement
-- and stamped disbursed_at = NOW(), but nothing ever asked a bank to move the
-- money. The worker was told they had a grant and never received it; the
-- ledger said cash had left when it hadn't, which the hourly reconciliation
-- would surface as drift. This is the same gap migration 000017 closed for
-- EWA advances, closed the same way: the grant is submitted to a payment
-- provider by a worker task, and the provider's webhook confirms it
-- (disbursed) or reports failure (failed, ledger reversed).
--
-- Existing rows keep status 'disbursed'. That is what the system has
-- already told the admin, and automatically re-sending money for historic
-- rows could double-pay anyone an operator already paid by hand. Operators
-- should reconcile any pre-existing grants manually.

BEGIN;

ALTER TABLE ewa_hardship_grants
    ADD COLUMN IF NOT EXISTS status             TEXT NOT NULL DEFAULT 'disbursed',
    ADD COLUMN IF NOT EXISTS provider_name      TEXT,
    ADD COLUMN IF NOT EXISTS provider_reference TEXT,
    ADD COLUMN IF NOT EXISTS submitted_at       TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS failure_reason     TEXT;

-- New grants start pending; disbursed_at is set only on confirmation.
ALTER TABLE ewa_hardship_grants ALTER COLUMN status SET DEFAULT 'pending';
ALTER TABLE ewa_hardship_grants ALTER COLUMN disbursed_at DROP NOT NULL;
ALTER TABLE ewa_hardship_grants ALTER COLUMN disbursed_at DROP DEFAULT;

ALTER TABLE ewa_hardship_grants DROP CONSTRAINT IF EXISTS ewa_hardship_grants_status_check;
ALTER TABLE ewa_hardship_grants ADD CONSTRAINT ewa_hardship_grants_status_check
    CHECK (status IN ('pending', 'submitted', 'disbursed', 'failed'));

-- Same (provider, reference) uniqueness as ewa_advances — see 000017.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ewa_hardship_grants_provider_reference
    ON ewa_hardship_grants (provider_name, provider_reference)
    WHERE provider_reference IS NOT NULL;

-- The webhook carries our reference (the grant ID) but no org_id, so the row
-- must be found before RLS has anything to scope by. Same SECURITY DEFINER,
-- one-row lookup as lookup_ewa_advance_for_webhook, with the same
-- justification: HMAC authenticates the request before any DB access, and
-- every write afterwards runs inside WithOrgScope with the org_id this
-- lookup reveals.
CREATE OR REPLACE FUNCTION lookup_hardship_grant_for_webhook(p_ref TEXT)
RETURNS SETOF ewa_hardship_grants
LANGUAGE sql
SECURITY DEFINER
STABLE
SET search_path = public, pg_temp
AS $$
    SELECT * FROM ewa_hardship_grants WHERE id = p_ref LIMIT 1;
$$;

GRANT EXECUTE ON FUNCTION lookup_hardship_grant_for_webhook(TEXT) TO PUBLIC;

COMMIT;
