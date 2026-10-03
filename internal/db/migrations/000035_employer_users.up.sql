-- 000035_employer_users.up.sql
-- Per-person employer identity. Until now an employer "login" was one shared
-- password per organization, so every audit row said "admin" and nobody could
-- be removed without rotating the password for everyone.
--
-- Expand-only: organizations.password_hash keeps working. Re-runnable.

CREATE TABLE IF NOT EXISTS employer_users (
    id                   TEXT PRIMARY KEY,
    organization_id      TEXT NOT NULL REFERENCES organizations(id),
    email                TEXT NOT NULL,
    name                 TEXT NOT NULL DEFAULT '',
    password_hash        TEXT NOT NULL,
    role                 TEXT NOT NULL DEFAULT 'viewer',
    is_active            BOOLEAN NOT NULL DEFAULT TRUE,
    must_change_password BOOLEAN NOT NULL DEFAULT FALSE,
    last_login_at        TIMESTAMPTZ,
    password_changed_at  TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Login is by email alone, so an email identifies exactly one person.
CREATE UNIQUE INDEX IF NOT EXISTS uq_employer_users_email ON employer_users (lower(email));
CREATE INDEX IF NOT EXISTS idx_employer_users_org ON employer_users (organization_id);

DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'employer_users_role_check') THEN
        ALTER TABLE employer_users ADD CONSTRAINT employer_users_role_check
            CHECK (role IN ('admin', 'viewer', 'compliance'));
    END IF;
END $$;

ALTER TABLE employer_users ENABLE ROW LEVEL SECURITY;
ALTER TABLE employer_users FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS employer_users_org_isolation ON employer_users;
CREATE POLICY employer_users_org_isolation ON employer_users
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

-- Login must find the person before the org is known. One row, exact
-- (case-insensitive) email, nothing else exposed. Same pattern as 000011.
CREATE OR REPLACE FUNCTION find_employer_user_for_login(p_email TEXT)
RETURNS SETOF employer_users
LANGUAGE sql
SECURITY DEFINER
STABLE
SET search_path = public, pg_temp
AS $$
    SELECT * FROM employer_users WHERE lower(email) = lower(p_email) LIMIT 1;
$$;
GRANT EXECUTE ON FUNCTION find_employer_user_for_login(TEXT) TO PUBLIC;
