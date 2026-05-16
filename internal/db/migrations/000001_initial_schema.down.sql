-- 000001_initial_schema.down.sql
-- Tears it all down. Only run this if you really mean it.

DROP INDEX IF EXISTS idx_audit_org;
DROP INDEX IF EXISTS idx_audit_created;
DROP INDEX IF EXISTS idx_audit_entity;
DROP TABLE IF EXISTS audit_events;

DROP INDEX IF EXISTS idx_payroll_items_org;
DROP INDEX IF EXISTS idx_payroll_items_employee;
DROP INDEX IF EXISTS idx_payroll_items_payroll;
DROP TABLE IF EXISTS payroll_items;

DROP INDEX IF EXISTS idx_payrolls_org;
DROP INDEX IF EXISTS idx_payrolls_period_org;
DROP TABLE IF EXISTS payrolls;

DROP INDEX IF EXISTS idx_employees_org;
DROP INDEX IF EXISTS idx_employees_email;
DROP TABLE IF EXISTS employees;

DROP TABLE IF EXISTS organizations;
