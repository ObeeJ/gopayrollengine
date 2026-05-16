-- 000004_users_and_advances.up.sql
-- Worker identity (User) and EWA advance request tables.
-- The advance request table stores the data shape only.
-- Discipline gate enforcement lives in the private product repository.

CREATE TABLE IF NOT EXISTS users (
    id            TEXT PRIMARY KEY,
    employee_id   TEXT NOT NULL REFERENCES employees(id),
    org_id        TEXT NOT NULL REFERENCES organizations(id),
    phone         TEXT NOT NULL,
    is_active     BOOLEAN NOT NULL DEFAULT TRUE,
    last_login_at TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ,
    UNIQUE(employee_id),
    UNIQUE(phone)
);

CREATE INDEX IF NOT EXISTS idx_users_org ON users(org_id);

CREATE TABLE IF NOT EXISTS advance_requests (
    id            TEXT PRIMARY KEY,
    org_id        TEXT NOT NULL REFERENCES organizations(id),
    employee_id   TEXT NOT NULL REFERENCES employees(id),
    user_id       TEXT NOT NULL REFERENCES users(id),
    amount        NUMERIC(20,2) NOT NULL DEFAULT 0,
    reason        TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'pending',
    payday_target TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_advances_employee ON advance_requests(employee_id);
CREATE INDEX IF NOT EXISTS idx_advances_org ON advance_requests(org_id);
CREATE INDEX IF NOT EXISTS idx_advances_status ON advance_requests(status);
