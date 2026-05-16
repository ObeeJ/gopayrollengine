-- 000003_data_residency.down.sql
DROP INDEX IF EXISTS idx_orgs_region;
ALTER TABLE organizations DROP COLUMN IF EXISTS data_region;
