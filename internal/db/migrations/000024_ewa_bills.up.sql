-- 000024_ewa_bills.up.sql
-- Phase 4 roadmap item: "Bill-timing tools — much repeat usage is a timing
-- mismatch, not a shortfall."
--
-- WHY THIS EXISTS
-- A worker who draws early pay every month right before rent is due doesn't
-- necessarily lack money — the wages to cover rent may already be earned,
-- just not yet paid, because payday and the bill's due date don't line up.
-- Treating every draw as "not enough money" misdiagnoses a scheduling
-- problem as a shortfall. This table lets a worker record their recurring
-- bills so the product can name that mismatch explicitly and size a draw to
-- close exactly the gap, rather than leaving them to guess an amount.

BEGIN;

CREATE TABLE IF NOT EXISTS ewa_bills (
    id               TEXT PRIMARY KEY,
    organization_id  TEXT NOT NULL REFERENCES organizations(id),
    employee_id      TEXT NOT NULL REFERENCES employees(id),

    name             TEXT NOT NULL,
    amount_kobo      BIGINT NOT NULL CHECK (amount_kobo > 0),
    -- Day of the month the bill is due, 1-31. A month shorter than the
    -- chosen day (e.g. 31 in a 30-day month) is resolved to that month's
    -- last day at query time, never stored differently per month.
    due_day          INTEGER NOT NULL CHECK (due_day BETWEEN 1 AND 31),
    active           BOOLEAN NOT NULL DEFAULT true,

    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ewa_bills_employee ON ewa_bills(organization_id, employee_id) WHERE active;

ALTER TABLE ewa_bills ENABLE ROW LEVEL SECURITY;
ALTER TABLE ewa_bills FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ewa_bills_org_isolation ON ewa_bills;
CREATE POLICY ewa_bills_org_isolation ON ewa_bills
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

COMMIT;
