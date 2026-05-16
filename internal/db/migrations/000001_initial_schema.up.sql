-- 000001_initial_schema.up.sql
-- The foundation. Every table that will ever hold money-adjacent data starts here.

CREATE TABLE IF NOT EXISTS organizations (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at  TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS employees (
    id              TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id),
    name            TEXT NOT NULL,
    email           TEXT NOT NULL,
    account_number  TEXT NOT NULL,  -- AES-256-GCM ciphertext; never plaintext
    bank_code       TEXT NOT NULL,  -- AES-256-GCM ciphertext; never plaintext
    salary          NUMERIC(20,2) NOT NULL DEFAULT 0,
    is_active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_employees_email ON employees(email) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_employees_org ON employees(organization_id);

CREATE TABLE IF NOT EXISTS payrolls (
    id              TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id),
    period          TEXT NOT NULL,
    total_amount    NUMERIC(20,2) NOT NULL DEFAULT 0,
    status          TEXT NOT NULL DEFAULT 'pending',
    pending_count   INTEGER NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at      TIMESTAMPTZ
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_payrolls_period_org ON payrolls(organization_id, period) WHERE deleted_at IS NULL;
CREATE INDEX IF NOT EXISTS idx_payrolls_org ON payrolls(organization_id);

CREATE TABLE IF NOT EXISTS payroll_items (
    id                    TEXT PRIMARY KEY,
    organization_id       TEXT NOT NULL REFERENCES organizations(id),
    payroll_id            TEXT NOT NULL REFERENCES payrolls(id),
    employee_id           TEXT NOT NULL REFERENCES employees(id),
    employee_name         TEXT NOT NULL,
    amount                NUMERIC(20,2) NOT NULL DEFAULT 0,
    status                TEXT NOT NULL DEFAULT 'pending',
    transaction_reference TEXT,
    error_message         TEXT,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at            TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_payroll_items_payroll ON payroll_items(payroll_id);
CREATE INDEX IF NOT EXISTS idx_payroll_items_employee ON payroll_items(employee_id);
CREATE INDEX IF NOT EXISTS idx_payroll_items_org ON payroll_items(organization_id);

CREATE TABLE IF NOT EXISTS audit_events (
    id              TEXT PRIMARY KEY,
    organization_id TEXT,
    entity_type     TEXT NOT NULL,
    entity_id       TEXT NOT NULL,
    action          TEXT NOT NULL,
    before          TEXT,
    after           TEXT,
    actor_ip        TEXT,
    actor_key       TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
    -- No updated_at, no deleted_at — this table is append-only by design.
    -- If you're tempted to add UPDATE here, step away from the keyboard.
);

CREATE INDEX IF NOT EXISTS idx_audit_entity ON audit_events(entity_type, entity_id);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_events(created_at);
CREATE INDEX IF NOT EXISTS idx_audit_org ON audit_events(organization_id);
