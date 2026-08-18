-- 000017_ewa_disbursement.down.sql
BEGIN;

DROP FUNCTION IF EXISTS lookup_ewa_advance_for_webhook(TEXT);
DROP INDEX IF EXISTS idx_ewa_advances_provider_reference;
ALTER TABLE ewa_advances
    DROP COLUMN IF EXISTS provider_name,
    DROP COLUMN IF EXISTS provider_reference;

COMMIT;
