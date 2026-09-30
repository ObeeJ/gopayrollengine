BEGIN;

DROP FUNCTION IF EXISTS lookup_hardship_grant_for_webhook(TEXT);
DROP INDEX IF EXISTS idx_ewa_hardship_grants_provider_reference;
ALTER TABLE ewa_hardship_grants DROP CONSTRAINT IF EXISTS ewa_hardship_grants_status_check;

UPDATE ewa_hardship_grants SET disbursed_at = created_at WHERE disbursed_at IS NULL;
ALTER TABLE ewa_hardship_grants ALTER COLUMN disbursed_at SET DEFAULT NOW();
ALTER TABLE ewa_hardship_grants ALTER COLUMN disbursed_at SET NOT NULL;

ALTER TABLE ewa_hardship_grants
    DROP COLUMN IF EXISTS failure_reason,
    DROP COLUMN IF EXISTS submitted_at,
    DROP COLUMN IF EXISTS provider_reference,
    DROP COLUMN IF EXISTS provider_name,
    DROP COLUMN IF EXISTS status;

COMMIT;
