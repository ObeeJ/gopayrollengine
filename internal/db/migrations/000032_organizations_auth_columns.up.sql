-- 000032_organizations_auth_columns.up.sql
--
-- Migration 000002 intended to add password_hash, role, and is_active to
-- organizations via "CREATE TABLE IF NOT EXISTS organizations (...)" — but
-- 000001 had already created that table without them, so IF NOT EXISTS made
-- 000002's CREATE TABLE a silent no-op. These columns have never actually
-- existed on any database that ran migrations from a clean state (dev, CI,
-- and presumably every real deployment). Nothing surfaced this until now
-- because every Organization row in this codebase is created via a raw SQL
-- INSERT that never touches password_hash (see e.g. every test's
-- seed*Worker helper) — D2C self-serve signup is the first caller that
-- creates an Organization through the GORM model itself, PasswordHash
-- included, and it fails outright against a real migrated schema without
-- this fix.
ALTER TABLE organizations
    ADD COLUMN IF NOT EXISTS password_hash TEXT NOT NULL DEFAULT '',
    ADD COLUMN IF NOT EXISTS role TEXT NOT NULL DEFAULT 'admin',
    ADD COLUMN IF NOT EXISTS is_active BOOLEAN NOT NULL DEFAULT TRUE;

-- An empty password_hash never validates — bcrypt.CompareHashAndPassword
-- rejects it as not a well-formed hash — so any organization row that
-- predates this column (every one there's ever been, until now) fails
-- closed on login rather than accepting a blank password.
COMMENT ON COLUMN organizations.password_hash IS
    'bcrypt hash. Empty means no employer login exists for this org (e.g. a D2C org, which has none by design) — an empty value never validates, it never means "no password required".';
