-- 000010_rls_strict.up.sql
-- Tightens the RLS policies installed in 000008 and 000009 by removing the
-- permissive bypass branch (app_current_org_id() = '') that existed to give
-- code paths time to adopt models.WithOrgScope. All handler families,
-- services, and the payroll worker now wrap their DB work in WithOrgScope, so
-- queries that forget to set app.org_id will see zero rows instead of every
-- tenant's data — the intended invariant.
--
-- payroll_items is intentionally left with its bypass clause intact. The
-- Monnify webhook handler must look up a PayrollItem by UUID (the transaction
-- reference) *before* it knows which org owns it, because orgID is not present
-- in the Monnify callback payload. That single unscoped read is safe because:
--   (a) the request is authenticated by HMAC-SHA512 before any DB access, and
--   (b) all subsequent writes (status transition, counter decrement, audit) run
--       inside models.WithOrgScope with the orgID extracted from the loaded row.
-- Once Monnify supports org-aware callbacks, or an alternative lookup strategy
-- is implemented, the bypass on payroll_items can be dropped too.

BEGIN;

-- employees —— all writes and reads now go through WithOrgScope
DROP POLICY IF EXISTS employees_org_isolation ON employees;
CREATE POLICY employees_org_isolation ON employees
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

-- payrolls
DROP POLICY IF EXISTS payrolls_org_isolation ON payrolls;
CREATE POLICY payrolls_org_isolation ON payrolls
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

-- payroll_items — bypass retained for webhook UUID lookup (see above)
-- DROP POLICY IF EXISTS payroll_items_org_isolation ON payroll_items;
-- CREATE POLICY payroll_items_org_isolation ON payroll_items
--     USING (organization_id = app_current_org_id())
--     WITH CHECK (organization_id = app_current_org_id());

-- advance_requests
DROP POLICY IF EXISTS advance_requests_org_isolation ON advance_requests;
CREATE POLICY advance_requests_org_isolation ON advance_requests
    USING (org_id = app_current_org_id())
    WITH CHECK (org_id = app_current_org_id());

-- audit_events — NULL-org rows (system events) remain visible to all sessions
DROP POLICY IF EXISTS audit_events_org_isolation ON audit_events;
CREATE POLICY audit_events_org_isolation ON audit_events
    USING (organization_id IS NULL OR organization_id = app_current_org_id())
    WITH CHECK (organization_id IS NULL OR organization_id = app_current_org_id());

-- consent_records
DROP POLICY IF EXISTS consent_records_org_isolation ON consent_records;
CREATE POLICY consent_records_org_isolation ON consent_records
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

-- bvn_verifications
DROP POLICY IF EXISTS bvn_verifications_org_isolation ON bvn_verifications;
CREATE POLICY bvn_verifications_org_isolation ON bvn_verifications
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

COMMIT;
