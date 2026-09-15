-- 000027_payroll_shift_overtime_rules.up.sql
-- Phase 5 roadmap item: "Shift-differential/overtime pay rules for hourly
-- accrual."
--
-- WHY THIS EXISTS
-- Hourly gross today is a flat rate × approved minutes (payroll_service.go).
-- That's wrong wherever a night, weekend, or holiday shift pays a premium, or
-- where a week's hours cross a statutory overtime threshold. This migration
-- adds the two pieces needed to fix it: which differential a logged shift
-- claims (time_entries.shift_type — self-declared at submission, since
-- work_date is a DATE with no time-of-day to infer "night" from), and the
-- per-org rules for how much that's worth (payroll_policies). See
-- services.ComputeHourlyGross for the computation that reads both.
--
-- Multipliers are basis points (10000 = 1.0x), matching the existing
-- max_accrual_pct-style percent convention in this codebase, and are floored
-- at 10000 by CHECK: a "differential" below the base rate isn't a
-- differential, and this table must never be capable of silently cutting pay.

BEGIN;

ALTER TABLE time_entries
    ADD COLUMN IF NOT EXISTS shift_type TEXT NOT NULL DEFAULT 'regular'
    CHECK (shift_type IN ('regular', 'night', 'weekend', 'holiday'));

CREATE TABLE IF NOT EXISTS payroll_policies (
    organization_id TEXT PRIMARY KEY REFERENCES organizations(id),

    -- Calendar-week (Mon-Sun) minute count beyond which further approved
    -- minutes in that week are overtime. 2400 = 40h.
    overtime_threshold_minutes_per_week INTEGER NOT NULL DEFAULT 2400
                                        CHECK (overtime_threshold_minutes_per_week > 0),
    -- Applied only to the portion of a week's minutes past the threshold, as
    -- an ADDITIONAL amount on top of whatever shift differential already
    -- applies to those minutes — never a replacement for it. 15000 =
    -- time-and-a-half.
    overtime_multiplier_bps             INTEGER NOT NULL DEFAULT 15000
                                        CHECK (overtime_multiplier_bps >= 10000),

    night_shift_multiplier_bps          INTEGER NOT NULL DEFAULT 10000
                                        CHECK (night_shift_multiplier_bps >= 10000),
    weekend_shift_multiplier_bps        INTEGER NOT NULL DEFAULT 10000
                                        CHECK (weekend_shift_multiplier_bps >= 10000),
    holiday_shift_multiplier_bps        INTEGER NOT NULL DEFAULT 10000
                                        CHECK (holiday_shift_multiplier_bps >= 10000),

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE payroll_policies ENABLE ROW LEVEL SECURITY;
ALTER TABLE payroll_policies FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS payroll_policies_org_isolation ON payroll_policies;
CREATE POLICY payroll_policies_org_isolation ON payroll_policies
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

COMMIT;
