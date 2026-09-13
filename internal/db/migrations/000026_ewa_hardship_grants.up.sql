-- 000026_ewa_hardship_grants.up.sql
-- Phase 4 roadmap item: "Employer-funded hardship grants as a genuine
-- alternative to a fourth advance."
--
-- WHY THIS EXISTS
-- Hitting the per-period draw limit while already Strained or Dependent is
-- exactly the moment a fifth (or fourth) advance helps least — it recovers
-- from a future payday that is already spoken for. A grant is the opposite
-- of an advance in the one way that matters here: it is never recovered
-- from payroll. This table records that an admin actually approved one —
-- discretionary employer money, so someone has to decide, the same reason
-- POST /employees/:id/terminate and payroll creation are admin-only.
--
-- Ledger treatment (see EWAService.IssueHardshipGrant): Dr
-- hardship_grant_expense / Cr cash_settlement — an immediate, permanent
-- expense, never a receivable. No new AccountType migration is needed since
-- ledger_accounts.account_type carries no CHECK constraint (see 000013).

BEGIN;

CREATE TABLE IF NOT EXISTS ewa_hardship_grants (
    id               TEXT PRIMARY KEY,
    organization_id  TEXT NOT NULL REFERENCES organizations(id),
    employee_id      TEXT NOT NULL REFERENCES employees(id),

    amount_kobo      BIGINT NOT NULL CHECK (amount_kobo > 0),
    reason           TEXT NOT NULL,
    approved_by_ip   TEXT NOT NULL,

    disbursed_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ewa_hardship_grants_employee ON ewa_hardship_grants(organization_id, employee_id);

ALTER TABLE ewa_hardship_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE ewa_hardship_grants FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ewa_hardship_grants_org_isolation ON ewa_hardship_grants;
CREATE POLICY ewa_hardship_grants_org_isolation ON ewa_hardship_grants
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

COMMIT;
