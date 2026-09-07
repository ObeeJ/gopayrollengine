-- 000019_employer_funding_pool.up.sql
-- Closes a gap flagged in docs/EWA_ROADMAP.md Phase 2 as something that "must
-- be closed before any pilot": nothing today checks that an employer has
-- actually funded the EWA pool before their workers draw against it.
-- AccountEmployerFunding has existed in the chart of accounts since the
-- ledger was built (see internal/models/ledger.go) but nothing has ever
-- posted to it — RequestAdvance/postAdvanceLedger draws straight against
-- AccountCashSettlement with no funding check at all.
--
-- DESIGN
-- postAdvanceLedger's existing entries (Dr advance_receivable / Cr
-- cash_settlement) are deliberately left untouched — that path is already
-- shipped and tested, and rewriting its meaning this late is a bigger risk
-- than the guardrail is worth. Instead, an employer deposit posts the other
-- half of the same account: Dr cash_settlement / Cr employer_funding. That
-- makes cash_settlement's own balance (Credit-normal) exactly the number a
-- funding check needs: cumulative advances disbursed minus cumulative
-- employer deposits. Zero or negative means the employer has funded at least
-- as much as has gone out; positive means they haven't. No new account type,
-- no change to the advance-posting path.
--
-- The check is opt-in per org (ewa_policies.require_funding_coverage,
-- default FALSE) rather than a hard cutover: every org already running EWA
-- today has zero deposits recorded, and flipping this on unconditionally
-- would block every one of them from any further draw the moment this ships.
--
-- organization_funding_accounts holds the dedicated virtual account number
-- (Monnify "reserved account", or equivalent) each org deposits into. The
-- webhook-lookup problem is the same one ewa_advances and payroll_items
-- solved: a deposit notification carries the account reference, not an
-- org_id, so the row must be found before RLS has anything to scope by —
-- same SECURITY DEFINER pattern as lookup_ewa_advance_for_webhook.

BEGIN;

CREATE TABLE IF NOT EXISTS organization_funding_accounts (
    organization_id   TEXT PRIMARY KEY REFERENCES organizations(id),
    provider_name     TEXT NOT NULL,

    -- Our own reference, sent to the provider when the account was created —
    -- distinct from account_number because a provider's webhook may key its
    -- deposit notification off either one depending on the rail.
    account_reference TEXT NOT NULL,
    account_number    TEXT NOT NULL,
    account_name      TEXT NOT NULL,
    bank_name         TEXT NOT NULL,
    bank_code         TEXT,

    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_org_funding_accounts_reference
    ON organization_funding_accounts (provider_name, account_reference);

ALTER TABLE organization_funding_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE organization_funding_accounts FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS organization_funding_accounts_org_isolation ON organization_funding_accounts;
CREATE POLICY organization_funding_accounts_org_isolation ON organization_funding_accounts
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

CREATE OR REPLACE FUNCTION lookup_org_for_funding_account(p_account_reference TEXT)
RETURNS SETOF organization_funding_accounts
LANGUAGE sql
SECURITY DEFINER
STABLE
SET search_path = public, pg_temp
AS $$
    SELECT * FROM organization_funding_accounts WHERE account_reference = p_account_reference LIMIT 1;
$$;

GRANT EXECUTE ON FUNCTION lookup_org_for_funding_account(TEXT) TO PUBLIC;

-- Off by default — see design note above.
ALTER TABLE ewa_policies ADD COLUMN IF NOT EXISTS require_funding_coverage BOOLEAN NOT NULL DEFAULT FALSE;

COMMIT;
