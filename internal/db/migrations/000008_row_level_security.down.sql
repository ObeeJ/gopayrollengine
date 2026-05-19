-- 000008_row_level_security.down.sql

BEGIN;

DROP POLICY IF EXISTS employees_org_isolation ON employees;
ALTER TABLE employees NO FORCE ROW LEVEL SECURITY;
ALTER TABLE employees DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS payrolls_org_isolation ON payrolls;
ALTER TABLE payrolls NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payrolls DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS payroll_items_org_isolation ON payroll_items;
ALTER TABLE payroll_items NO FORCE ROW LEVEL SECURITY;
ALTER TABLE payroll_items DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS advance_requests_org_isolation ON advance_requests;
ALTER TABLE advance_requests NO FORCE ROW LEVEL SECURITY;
ALTER TABLE advance_requests DISABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS audit_events_org_isolation ON audit_events;
ALTER TABLE audit_events NO FORCE ROW LEVEL SECURITY;
ALTER TABLE audit_events DISABLE ROW LEVEL SECURITY;

DROP FUNCTION IF EXISTS app_current_org_id();

COMMIT;
