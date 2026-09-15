-- 000030_d2c_bank_links.up.sql
-- Phase 5 roadmap item: "direct-to-consumer EWA (no payroll deduction)."
--
-- WHY THIS EXISTS
-- Every EWA advance to date settles by netting out of the worker's NEXT
-- PAYROLL ITEM (SettleAdvancesForPayrollItem) — that IS the entire recourse
-- mechanism, and the reason this product can credibly claim to be
-- structurally not lending (docs/EWA_ROADMAP.md §7: "recovery only by
-- payroll deduction — no recourse to the worker"). A direct-to-consumer
-- worker has no payroll relationship in this system at all: no employer
-- configured their salary, no admin runs payroll for them. There is nothing
-- to net an advance against.
--
-- The repayment mechanism for D2C is therefore fundamentally different:
-- direct debit from the worker's own bank account, authorized in advance,
-- pulled on their predicted payday (estimated from their own observed
-- deposit history — see services.PredictNextPayday, added alongside this
-- migration). That is real recourse and a real collections relationship,
-- which changes this product line's regulatory posture from the org-funded
-- model's — see docs/EWA_ROADMAP.md §7's new D2C note. organizations.is_d2c
-- exists so that distinction is explicit and queryable everywhere, not an
-- implicit convention nobody enforces.
--
-- d2c_bank_links only records that a link exists and how to reach it
-- (provider + provider's own account reference) — never account numbers,
-- balances, or raw transaction data. Transaction history for payday
-- prediction is fetched live from the provider each time, not mirrored here;
-- storing a consumer's full transaction history would be a second PII
-- surface this migration deliberately avoids creating.
--
-- Org-scoped, not user-scoped, like every other table in this schema
-- (see CLAUDE.md's RLS section) — a D2C worker is modelled as a
-- single-employee organization (is_d2c = true), the same tenant boundary
-- every other table already assumes, rather than inventing a parallel
-- non-RLS'd identity model for one product line.

BEGIN;

ALTER TABLE organizations
    ADD COLUMN IF NOT EXISTS is_d2c BOOLEAN NOT NULL DEFAULT FALSE;

CREATE TABLE IF NOT EXISTS d2c_bank_links (
    id                   TEXT PRIMARY KEY,
    organization_id      TEXT NOT NULL REFERENCES organizations(id),
    employee_id          TEXT NOT NULL REFERENCES employees(id),

    provider             TEXT NOT NULL,              -- "mono" | "okra" | ... (internal/integrations/banklink)
    provider_account_ref TEXT NOT NULL,               -- provider's own identifier for the linked account
    status               TEXT NOT NULL DEFAULT 'linked'
                          CHECK (status IN ('linked', 'revoked')),

    linked_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at           TIMESTAMPTZ,
    last_synced_at       TIMESTAMPTZ,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One active link per employee per provider — relinking the same provider
-- account revokes the old row rather than leaving two "linked" rows to
-- choose between.
CREATE UNIQUE INDEX IF NOT EXISTS idx_d2c_bank_links_active
    ON d2c_bank_links (organization_id, employee_id, provider)
    WHERE status = 'linked';

CREATE INDEX IF NOT EXISTS idx_d2c_bank_links_employee
    ON d2c_bank_links (organization_id, employee_id);

ALTER TABLE d2c_bank_links ENABLE ROW LEVEL SECURITY;
ALTER TABLE d2c_bank_links FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS d2c_bank_links_org_isolation ON d2c_bank_links;
CREATE POLICY d2c_bank_links_org_isolation ON d2c_bank_links
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

COMMIT;
