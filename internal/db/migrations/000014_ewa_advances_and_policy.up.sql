-- 000014_ewa_advances_and_policy.up.sql
-- Earned Wage Access: accrual snapshots, per-org policy, and the advance record.
--
-- This supersedes advance_requests, which stored a shape but enforced nothing:
-- it had no link to earned wages, no settlement path back into payroll, and no
-- ledger entries. That table is left in place (deprecated) so existing rows are
-- not destroyed; all new writes go to ewa_advances.
--
-- Product invariants encoded here rather than in application code:
--   * An advance can only ever be a claim on wages ALREADY earned. The cap is
--     recomputed server-side at request time; a client cannot supply it.
--   * Advances carry no interest. fee_kobo exists so employer-funded models can
--     record a fee where one is genuinely charged, and defaults to zero.
--   * There is no recourse beyond payroll deduction — no collections, no credit
--     reporting. Encoded as an absent capability, which is the only durable way
--     to encode it.

BEGIN;

-- Per-organization guardrail policy -------------------------------------------
CREATE TABLE IF NOT EXISTS ewa_policies (
    organization_id      TEXT PRIMARY KEY REFERENCES organizations(id),
    enabled              BOOLEAN NOT NULL DEFAULT TRUE,

    -- Share of wages accrued so far that a worker may draw. Deliberately below
    -- 100%: leaving headroom is what keeps payday from arriving at zero.
    max_accrual_pct      INTEGER NOT NULL DEFAULT 50
                         CHECK (max_accrual_pct BETWEEN 1 AND 100),

    -- Hard ceiling regardless of salary, in kobo. Default ₦200,000.
    absolute_cap_kobo    BIGINT  NOT NULL DEFAULT 20000000 CHECK (absolute_cap_kobo > 0),
    -- Floor per draw, in kobo. Default ₦1,000 — stops fee-free micro-draws from
    -- becoming a habit loop.
    min_draw_kobo        BIGINT  NOT NULL DEFAULT 100000   CHECK (min_draw_kobo > 0),

    -- Velocity guardrails.
    max_draws_per_period INTEGER NOT NULL DEFAULT 4  CHECK (max_draws_per_period > 0),
    cooling_off_hours    INTEGER NOT NULL DEFAULT 24 CHECK (cooling_off_hours >= 0),

    -- Even a worker flagged as dependent keeps access to this much. Blocking
    -- outright pushes people to payday lenders at multiples of the cost; the
    -- floor is a harm-reduction control, not a generosity setting.
    emergency_floor_kobo BIGINT  NOT NULL DEFAULT 500000 CHECK (emergency_floor_kobo >= 0),

    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Advances --------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ewa_advances (
    id                       TEXT PRIMARY KEY,
    organization_id          TEXT NOT NULL REFERENCES organizations(id),
    employee_id              TEXT NOT NULL REFERENCES employees(id),
    user_id                  TEXT REFERENCES users(id),

    -- Payroll period this advance settles against, e.g. '2026-08'.
    period                   TEXT   NOT NULL,
    amount_kobo              BIGINT NOT NULL CHECK (amount_kobo > 0),
    fee_kobo                 BIGINT NOT NULL DEFAULT 0 CHECK (fee_kobo >= 0),

    status                   TEXT NOT NULL DEFAULT 'requested'
                             CHECK (status IN ('requested','approved','disbursed',
                                               'settled','declined','cancelled','written_off')),

    -- Decision evidence, frozen at request time. Recomputing these later would
    -- give a different answer and make the decision unauditable.
    accrued_at_request_kobo  BIGINT  NOT NULL CHECK (accrued_at_request_kobo >= 0),
    available_at_request_kobo BIGINT NOT NULL CHECK (available_at_request_kobo >= 0),
    dependency_score         INTEGER NOT NULL DEFAULT 0 CHECK (dependency_score BETWEEN 0 AND 100),
    dependency_tier          TEXT    NOT NULL DEFAULT 'healthy'
                             CHECK (dependency_tier IN ('healthy','elevated','strained','dependent')),
    decline_reason           TEXT,

    idempotency_key          TEXT,
    settled_payroll_item_id  TEXT,

    requested_at             TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    disbursed_at             TIMESTAMPTZ,
    settled_at               TIMESTAMPTZ,
    created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at               TIMESTAMPTZ
);

-- A retried request must not create a second advance.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ewa_advances_idempotency
    ON ewa_advances (organization_id, employee_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL AND deleted_at IS NULL;

-- Drives both the outstanding-balance query and the settlement sweep.
CREATE INDEX IF NOT EXISTS idx_ewa_advances_outstanding
    ON ewa_advances (organization_id, employee_id, period, status)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_ewa_advances_requested_at
    ON ewa_advances (organization_id, employee_id, requested_at DESC);

-- Accrual snapshots -----------------------------------------------------------
-- One row per employee per day the accrual engine runs. Kept because the
-- eligibility decision must be reconstructable months later during a dispute.
CREATE TABLE IF NOT EXISTS ewa_accrual_snapshots (
    id              BIGSERIAL PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id),
    employee_id     TEXT NOT NULL REFERENCES employees(id),
    period          TEXT NOT NULL,
    as_of_date      DATE NOT NULL,
    accrued_kobo    BIGINT NOT NULL CHECK (accrued_kobo >= 0),
    salary_kobo     BIGINT NOT NULL CHECK (salary_kobo >= 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (organization_id, employee_id, as_of_date)
);

-- Tenant isolation ------------------------------------------------------------
ALTER TABLE ewa_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE ewa_policies FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ewa_policies_org_isolation ON ewa_policies;
CREATE POLICY ewa_policies_org_isolation ON ewa_policies
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

ALTER TABLE ewa_advances ENABLE ROW LEVEL SECURITY;
ALTER TABLE ewa_advances FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ewa_advances_org_isolation ON ewa_advances;
CREATE POLICY ewa_advances_org_isolation ON ewa_advances
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

ALTER TABLE ewa_accrual_snapshots ENABLE ROW LEVEL SECURITY;
ALTER TABLE ewa_accrual_snapshots FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ewa_accrual_snapshots_org_isolation ON ewa_accrual_snapshots;
CREATE POLICY ewa_accrual_snapshots_org_isolation ON ewa_accrual_snapshots
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

COMMIT;
