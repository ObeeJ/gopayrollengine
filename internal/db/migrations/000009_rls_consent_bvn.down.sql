-- 000009_rls_consent_bvn.down.sql

BEGIN;

DROP POLICY IF EXISTS consent_records_org_isolation ON consent_records;
ALTER TABLE consent_records NO FORCE ROW LEVEL SECURITY;
ALTER TABLE consent_records DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS bvn_verifications_org_isolation ON bvn_verifications;
ALTER TABLE bvn_verifications NO FORCE ROW LEVEL SECURITY;
ALTER TABLE bvn_verifications DISABLE ROW LEVEL SECURITY;

COMMIT;
