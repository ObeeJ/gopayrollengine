-- 000003_data_residency.up.sql
-- Adds data_region to organizations so each tenant's data can be geo-fenced.
-- Default 'ng' keeps existing orgs working without any data migration.

ALTER TABLE organizations
    ADD COLUMN IF NOT EXISTS data_region TEXT NOT NULL DEFAULT 'ng';

COMMENT ON COLUMN organizations.data_region IS
    'ISO region code: ng=Nigeria, eu=EU, us=US. Enforced by DataResidency middleware.';

CREATE INDEX IF NOT EXISTS idx_orgs_region ON organizations(data_region);
