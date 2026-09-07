-- 000019_employer_funding_pool.down.sql

BEGIN;

ALTER TABLE ewa_policies DROP COLUMN IF EXISTS require_funding_coverage;

DROP FUNCTION IF EXISTS lookup_org_for_funding_account(TEXT);

DROP TABLE IF EXISTS organization_funding_accounts;

COMMIT;
