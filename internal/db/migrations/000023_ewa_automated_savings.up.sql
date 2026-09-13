-- 000023_ewa_automated_savings.up.sql
-- Phase 4 roadmap item: "Automated savings: round-up or a fixed share
-- diverted at payroll."
--
-- WHY THIS EXISTS
-- The dependency score identifies people the product cannot help by simply
-- giving them more of the same. Automated savings is the opposite motion:
-- instead of advancing money before payday, it sets a little of payday
-- itself aside, opt-in and worker-controlled, the same way ProtectedPayday
-- (000016) is worker-controlled rather than system-imposed.
--
-- Two modes, matching the roadmap's own phrasing:
--   fixed_percent — a flat share of net pay (post-EWA-settlement) diverted
--                   every payroll run.
--   round_up      — net pay is rounded UP to the next round_up_to_kobo
--                   unit (e.g. the next whole ₦1,000) and the difference is
--                   diverted; a worker whose pay already lands on the round
--                   unit saves nothing that run, by design — round-up saves
--                   the "spare change", not a guaranteed amount.
-- Exactly one of fixed_percent / round_up_to_kobo is meaningful per mode;
-- the unused one is simply not read, not required to be zero.

BEGIN;

CREATE TABLE IF NOT EXISTS ewa_savings_preferences (
    organization_id  TEXT NOT NULL REFERENCES organizations(id),
    employee_id      TEXT NOT NULL REFERENCES employees(id),

    enabled          BOOLEAN NOT NULL DEFAULT false,
    mode             TEXT NOT NULL DEFAULT 'fixed_percent'
                     CHECK (mode IN ('fixed_percent', 'round_up')),

    -- Whole percentage points of net pay, fixed_percent mode only. Capped
    -- below 100: the product's own thesis is that payday should not arrive
    -- empty, so automated savings must not be able to zero out a paycheck
    -- any more than an EWA draw is allowed to.
    fixed_percent    INTEGER NOT NULL DEFAULT 0
                     CHECK (fixed_percent >= 0 AND fixed_percent <= 90),

    -- Minor units of the org's payroll currency, round_up mode only. 0 means
    -- "not configured" — round-up mode with this at 0 diverts nothing rather
    -- than dividing by zero.
    round_up_to_kobo BIGINT NOT NULL DEFAULT 0
                     CHECK (round_up_to_kobo >= 0),

    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (organization_id, employee_id)
);

ALTER TABLE ewa_savings_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE ewa_savings_preferences FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ewa_savings_preferences_org_isolation ON ewa_savings_preferences;
CREATE POLICY ewa_savings_preferences_org_isolation ON ewa_savings_preferences
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

COMMIT;
