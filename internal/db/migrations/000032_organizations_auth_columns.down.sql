-- 000032_organizations_auth_columns.down.sql
ALTER TABLE organizations
    DROP COLUMN IF EXISTS password_hash,
    DROP COLUMN IF EXISTS role,
    DROP COLUMN IF EXISTS is_active;
