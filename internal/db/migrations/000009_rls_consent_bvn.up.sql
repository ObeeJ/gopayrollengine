-- 000009_rls_consent_bvn.up.sql
-- Extends RLS coverage to the two remaining tenant-scoped tables that 000008
-- missed: consent_records (NDPR evidence) and bvn_verifications (KYC). The
-- compliance handler aggregates both per-org, so leaving them outside RLS
-- would have meant an unfiltered Count() returns cross-tenant rows even with
-- migration 000008 in force. Same conditional bypass as 000008 so existing
-- code paths that haven't migrated to WithOrgScope keep working.

BEGIN;

ALTER TABLE consent_records ENABLE ROW LEVEL SECURITY;
ALTER TABLE consent_records FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS consent_records_org_isolation ON consent_records;
CREATE POLICY consent_records_org_isolation ON consent_records
    USING (app_current_org_id() = '' OR organization_id = app_current_org_id())
    WITH CHECK (app_current_org_id() = '' OR organization_id = app_current_org_id());

ALTER TABLE bvn_verifications ENABLE ROW LEVEL SECURITY;
ALTER TABLE bvn_verifications FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS bvn_verifications_org_isolation ON bvn_verifications;
CREATE POLICY bvn_verifications_org_isolation ON bvn_verifications
    USING (app_current_org_id() = '' OR organization_id = app_current_org_id())
    WITH CHECK (app_current_org_id() = '' OR organization_id = app_current_org_id());

COMMIT;
