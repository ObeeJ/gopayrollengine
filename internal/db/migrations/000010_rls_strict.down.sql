-- 000010_rls_strict.down.sql
-- Restores the permissive bypass policies from 000008/000009.

BEGIN;

DROP POLICY IF EXISTS employees_org_isolation ON employees;
CREATE POLICY employees_org_isolation ON employees
    USING (app_current_org_id() = '' OR organization_id = app_current_org_id())
    WITH CHECK (app_current_org_id() = '' OR organization_id = app_current_org_id());

DROP POLICY IF EXISTS payrolls_org_isolation ON payrolls;
CREATE POLICY payrolls_org_isolation ON payrolls
    USING (app_current_org_id() = '' OR organization_id = app_current_org_id())
    WITH CHECK (app_current_org_id() = '' OR organization_id = app_current_org_id());

DROP POLICY IF EXISTS advance_requests_org_isolation ON advance_requests;
CREATE POLICY advance_requests_org_isolation ON advance_requests
    USING (app_current_org_id() = '' OR org_id = app_current_org_id())
    WITH CHECK (app_current_org_id() = '' OR org_id = app_current_org_id());

DROP POLICY IF EXISTS audit_events_org_isolation ON audit_events;
CREATE POLICY audit_events_org_isolation ON audit_events
    USING (
        app_current_org_id() = ''
        OR organization_id IS NULL
        OR organization_id = app_current_org_id()
    )
    WITH CHECK (
        app_current_org_id() = ''
        OR organization_id IS NULL
        OR organization_id = app_current_org_id()
    );

DROP POLICY IF EXISTS consent_records_org_isolation ON consent_records;
CREATE POLICY consent_records_org_isolation ON consent_records
    USING (app_current_org_id() = '' OR organization_id = app_current_org_id())
    WITH CHECK (app_current_org_id() = '' OR organization_id = app_current_org_id());

DROP POLICY IF EXISTS bvn_verifications_org_isolation ON bvn_verifications;
CREATE POLICY bvn_verifications_org_isolation ON bvn_verifications
    USING (app_current_org_id() = '' OR organization_id = app_current_org_id())
    WITH CHECK (app_current_org_id() = '' OR organization_id = app_current_org_id());

COMMIT;
