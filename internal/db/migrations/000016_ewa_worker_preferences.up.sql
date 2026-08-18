-- 000016_ewa_worker_preferences.up.sql
-- Lets a worker set their own floor: "protect this much of my next payday".
--
-- WHY THIS EXISTS
-- The guardrails in 000014 are all imposed BY the system ON the worker. The
-- evidence for that shape is weak: adjacent research (gambling self-exclusion)
-- consistently finds restrictions are accepted when people experience them as
-- protecting their own interests, and resisted when they read as an outside
-- judgement about their behaviour. There is no primary evidence that EWA users
-- accept a provider unilaterally cutting their limit because a model decided
-- they are "dependent".
--
-- A worker-set floor inverts the relationship. Instead of the system deciding
-- someone has drawn too much, the worker decides in advance how much of payday
-- must survive — and the system holds them to it. Same protective effect,
-- opposite psychology, and it needs no claim about the accuracy of the
-- dependency model.
--
-- The floor is a MINIMUM the system also enforces against itself: an increase
-- takes effect immediately (protecting more is always allowed), while lowering
-- it is subject to the same cooling-off as a draw, so it cannot be dropped in
-- the moment of need to unlock more cash. That asymmetry is the whole point;
-- without it the floor is decorative.

BEGIN;

CREATE TABLE IF NOT EXISTS ewa_worker_preferences (
    organization_id       TEXT NOT NULL REFERENCES organizations(id),
    employee_id           TEXT NOT NULL REFERENCES employees(id),

    -- Minor units of the org's payroll currency. 0 means "no self-imposed floor".
    protected_payday_minor BIGINT NOT NULL DEFAULT 0
                           CHECK (protected_payday_minor >= 0),

    -- When this floor was last CHANGED, in either direction. Set on every
    -- write, not only on a lowering. This is what the cooling-off check on a
    -- lowering compares against — using "last time it was lowered" instead
    -- would leave the very first lowering after a raise unprotected, since
    -- there would be no prior lowering to rate-limit against. Raising itself
    -- is never blocked by this column; it only gates a subsequent lowering.
    last_changed_at       TIMESTAMPTZ,

    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (organization_id, employee_id)
);

ALTER TABLE ewa_worker_preferences ENABLE ROW LEVEL SECURITY;
ALTER TABLE ewa_worker_preferences FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ewa_worker_preferences_org_isolation ON ewa_worker_preferences;
CREATE POLICY ewa_worker_preferences_org_isolation ON ewa_worker_preferences
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

-- Default access share: 30%, not 50%.
--
-- Nigerian employee cooperatives — the incumbent this product competes with, and
-- the reference point workers already understand — commonly cap salary advances
-- at 30% of basic pay with payroll recovery. Launching at 50% would be looser
-- than the culturally established norm, which is the wrong direction for a
-- product whose entire thesis is that payday should not arrive empty. It is far
-- easier to raise a cap after watching real usage than to claw back access
-- people have already built a budget around.
ALTER TABLE ewa_policies ALTER COLUMN max_accrual_pct SET DEFAULT 30;

COMMIT;
