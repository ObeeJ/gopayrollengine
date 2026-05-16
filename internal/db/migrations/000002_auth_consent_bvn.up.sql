-- 000002_auth_consent_bvn.up.sql
-- Phase 2 tables: multi-tenant auth, NDPR consent, and KYC verification.

CREATE TABLE IF NOT EXISTS organizations (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL DEFAULT 'admin',
    is_active     BOOLEAN NOT NULL DEFAULT TRUE,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at    TIMESTAMPTZ
);

-- NDPR Article 26: every employee's consent to data processing must be on record.
-- Append-only: withdrawal creates a new row with granted=false, never deletes the old one.
CREATE TABLE IF NOT EXISTS consent_records (
    id              TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id),
    employee_id     TEXT NOT NULL REFERENCES employees(id),
    consent_type    TEXT NOT NULL,  -- "payroll_processing" | "ewa_access" | "data_sharing"
    granted         BOOLEAN NOT NULL,
    ip_address      TEXT,
    user_agent      TEXT,
    consented_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_consent_employee ON consent_records(employee_id);
CREATE INDEX IF NOT EXISTS idx_consent_org ON consent_records(organization_id);
CREATE INDEX IF NOT EXISTS idx_consent_type ON consent_records(organization_id, employee_id, consent_type);

-- BVN verification: CBN requires KYC at employee creation; we store the outcome, not the BVN.
CREATE TABLE IF NOT EXISTS bvn_verifications (
    id              TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id),
    employee_id     TEXT NOT NULL REFERENCES employees(id),
    provider        TEXT NOT NULL,       -- "dojah" | "smile" | "prembly" | "mock"
    status          TEXT NOT NULL,       -- "verified" | "failed" | "pending"
    response_hash   TEXT NOT NULL,       -- SHA-256 of provider response; proves we checked
    verified_at     TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE(employee_id)                  -- one verification record per employee
);

CREATE INDEX IF NOT EXISTS idx_bvn_org ON bvn_verifications(organization_id);

-- Add foreign key from employees to organizations now that organizations table exists.
ALTER TABLE employees
    ADD CONSTRAINT fk_employees_org
    FOREIGN KEY (organization_id) REFERENCES organizations(id)
    NOT VALID; -- NOT VALID skips locking the table on large datasets; validate separately

ALTER TABLE payrolls
    ADD CONSTRAINT fk_payrolls_org
    FOREIGN KEY (organization_id) REFERENCES organizations(id)
    NOT VALID;

ALTER TABLE payroll_items
    ADD CONSTRAINT fk_payroll_items_org
    FOREIGN KEY (organization_id) REFERENCES organizations(id)
    NOT VALID;
