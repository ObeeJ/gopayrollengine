-- 000018_hourly_wage_accrual.up.sql
-- Real accrual for hourly and gig workers, from logged and approved timesheet
-- entries — not an approximation.
--
-- WHY THIS EXISTS
-- AccruedToDate (services/ewa_service.go) has always assumed monthly salaried
-- staff, straight-lined across working days. The roadmap (docs/EWA_ROADMAP.md
-- §Phase 5) and CLAUDE.md are explicit that hourly accrual "must not be faked":
-- there is no honest way to approximate what an hourly or gig worker has earned
-- without knowing the hours they actually worked. This migration adds the
-- primitive that makes it possible to know that: a timesheet.
--
-- DESIGN
-- Hours are stored as integer minutes, never as fractional float hours, for the
-- same reason money is stored as integer minor units — no rounding drift
-- accumulating across a worker's history. An entry starts 'pending' and must be
-- approved before it counts toward accrual, payroll, or EWA eligibility: an
-- unapproved timesheet entry is a claim, not evidence of wages earned, and EWA
-- in particular must never advance against a claim nobody has verified.

BEGIN;

-- Wage type is per-employee, not per-org: a single organization can run a
-- salaried back office and an hourly warehouse floor on the same payroll.
ALTER TABLE employees ADD COLUMN IF NOT EXISTS wage_type TEXT NOT NULL DEFAULT 'salaried'
    CHECK (wage_type IN ('salaried', 'hourly'));

-- Only meaningful for wage_type = 'hourly'; zero for salaried staff whose pay
-- is governed by the existing `salary` column instead.
ALTER TABLE employees ADD COLUMN IF NOT EXISTS hourly_rate_kobo BIGINT NOT NULL DEFAULT 0
    CHECK (hourly_rate_kobo >= 0);

CREATE TABLE IF NOT EXISTS time_entries (
    id                TEXT PRIMARY KEY,
    organization_id   TEXT NOT NULL REFERENCES organizations(id),
    employee_id       TEXT NOT NULL REFERENCES employees(id),

    -- The calendar day the work happened, in the org's local reckoning. Payroll
    -- periods (YYYY-MM) are derived from this the same way they already are for
    -- accrued salary, so an hourly worker's period boundary behaves identically
    -- to a salaried one.
    work_date         DATE NOT NULL,

    -- Integer minutes, not fractional hours — see migration comment above.
    -- Bounded at 24h: a single entry longer than that is a data error, not a
    -- long shift, and must be split or corrected rather than accepted.
    minutes_worked    INTEGER NOT NULL CHECK (minutes_worked > 0 AND minutes_worked <= 1440),

    status            TEXT NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'approved', 'rejected')),
    note              TEXT,
    rejection_reason  TEXT,
    approved_by       TEXT,
    approved_at       TIMESTAMPTZ,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    deleted_at        TIMESTAMPTZ
);

-- Drives the accrual sum: "approved minutes for this employee, this period, up
-- to some date" is the query on the EWA and payroll hot paths alike.
CREATE INDEX IF NOT EXISTS idx_time_entries_accrual
    ON time_entries (organization_id, employee_id, status, work_date)
    WHERE deleted_at IS NULL;

CREATE INDEX IF NOT EXISTS idx_time_entries_employee_recent
    ON time_entries (organization_id, employee_id, work_date DESC)
    WHERE deleted_at IS NULL;

ALTER TABLE time_entries ENABLE ROW LEVEL SECURITY;
ALTER TABLE time_entries FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS time_entries_org_isolation ON time_entries;
CREATE POLICY time_entries_org_isolation ON time_entries
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

COMMIT;
