-- 000008_row_level_security.up.sql
-- Structural tenant isolation via Postgres Row Level Security (Wave 2 #4).
--
-- Convention-based isolation (every query passes WHERE organization_id = $1)
-- only holds while every developer remembers to do it. RLS moves the
-- enforcement into the database: a query that *forgets* the WHERE clause sees
-- zero rows instead of the entire multi-tenant table. The guarantee no longer
-- depends on code discipline.
--
-- The policy below is intentionally permissive when the session variable is
-- unset, because retrofitting RLS over a running codebase: existing code
-- paths (workers, migrations, internal scripts) don't yet wrap their queries
-- in models.WithOrgScope and would see nothing under a strict policy. Once
-- every tenant-touching code path is on the helper, swap this for a strict
-- policy that drops the empty-string branch.
--
-- FORCE ROW LEVEL SECURITY applies the policy even to the table owner — the
-- application's normal connection role. Without FORCE, the owner bypasses
-- RLS and the policy has no effect.

BEGIN;

-- Stable helper so the policy expression is short and the cost is amortised
-- (Postgres caches stable function results within a query).
CREATE OR REPLACE FUNCTION app_current_org_id() RETURNS TEXT
LANGUAGE sql STABLE
AS $$
    SELECT coalesce(current_setting('app.org_id', true), '');
$$;

-- employees ------------------------------------------------------------------
ALTER TABLE employees ENABLE ROW LEVEL SECURITY;
ALTER TABLE employees FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS employees_org_isolation ON employees;
CREATE POLICY employees_org_isolation ON employees
    USING (app_current_org_id() = '' OR organization_id = app_current_org_id())
    WITH CHECK (app_current_org_id() = '' OR organization_id = app_current_org_id());

-- payrolls -------------------------------------------------------------------
ALTER TABLE payrolls ENABLE ROW LEVEL SECURITY;
ALTER TABLE payrolls FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS payrolls_org_isolation ON payrolls;
CREATE POLICY payrolls_org_isolation ON payrolls
    USING (app_current_org_id() = '' OR organization_id = app_current_org_id())
    WITH CHECK (app_current_org_id() = '' OR organization_id = app_current_org_id());

-- payroll_items --------------------------------------------------------------
ALTER TABLE payroll_items ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_items FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS payroll_items_org_isolation ON payroll_items;
CREATE POLICY payroll_items_org_isolation ON payroll_items
    USING (app_current_org_id() = '' OR organization_id = app_current_org_id())
    WITH CHECK (app_current_org_id() = '' OR organization_id = app_current_org_id());

-- advance_requests -----------------------------------------------------------
ALTER TABLE advance_requests ENABLE ROW LEVEL SECURITY;
ALTER TABLE advance_requests FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS advance_requests_org_isolation ON advance_requests;
CREATE POLICY advance_requests_org_isolation ON advance_requests
    USING (app_current_org_id() = '' OR org_id = app_current_org_id())
    WITH CHECK (app_current_org_id() = '' OR org_id = app_current_org_id());

-- audit_events ---------------------------------------------------------------
-- organization_id is nullable on this table (system events have no tenant).
-- NULL-org rows are visible regardless of the session variable.
ALTER TABLE audit_events ENABLE ROW LEVEL SECURITY;
ALTER TABLE audit_events FORCE ROW LEVEL SECURITY;
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

COMMIT;
