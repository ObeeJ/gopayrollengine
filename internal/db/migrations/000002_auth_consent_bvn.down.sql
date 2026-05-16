-- 000002_auth_consent_bvn.down.sql
-- Rolls back Phase 2 tables. Run only if you know what you're doing.

ALTER TABLE payroll_items DROP CONSTRAINT IF EXISTS fk_payroll_items_org;
ALTER TABLE payrolls DROP CONSTRAINT IF EXISTS fk_payrolls_org;
ALTER TABLE employees DROP CONSTRAINT IF EXISTS fk_employees_org;

DROP INDEX IF EXISTS idx_bvn_org;
DROP TABLE IF EXISTS bvn_verifications;

DROP INDEX IF EXISTS idx_consent_type;
DROP INDEX IF EXISTS idx_consent_org;
DROP INDEX IF EXISTS idx_consent_employee;
DROP TABLE IF EXISTS consent_records;

DROP TABLE IF EXISTS organizations;
