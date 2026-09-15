-- 000029_organization_currency.up.sql
-- Phase 5 roadmap item: "multi-currency, multi-country."
--
-- WHY THIS EXISTS
-- pkg/money already has a real, currency-tagged Money type; the ledger
-- (migration 000015) already posts per-account, per-transaction currency; the
-- payment-provider abstraction (internal/integrations/provider) already
-- routes by currency and fails loudly when nothing can settle one. What was
-- missing was the one fact that ties all of that together for a given
-- tenant: which currency does THIS organization actually operate in. Without
-- it, every EWA ledger posting, provider selection, and policy default fell
-- back to a bare NGN literal regardless of who the org actually pays.
--
-- currency is set once, implicitly, at org creation (defaulting existing and
-- future rows to NGN — this system's only market until now) and never
-- changed after: an org's employees, payroll history, and ledger accounts
-- are all denominated in it, and switching currency out from under that
-- history would corrupt the ledger's per-account currency invariant
-- (migration 000015's ledger_entry_currency_matches_account trigger) rather
-- than converting anything. The BEFORE UPDATE trigger below is defense in
-- depth for that: today's code never updates this column, but the ledger's
-- own append-only triggers exist for exactly the same reason — an invariant
-- worth enforcing in the database, not just in the one code path that
-- currently respects it.

BEGIN;

ALTER TABLE organizations
    ADD COLUMN IF NOT EXISTS currency TEXT NOT NULL DEFAULT 'NGN'
    CHECK (currency IN ('NGN', 'USD', 'GHS', 'KES', 'ZAR', 'XOF', 'GBP', 'EUR'));

CREATE OR REPLACE FUNCTION reject_organization_currency_change() RETURNS TRIGGER AS $$
BEGIN
    IF NEW.currency IS DISTINCT FROM OLD.currency THEN
        RAISE EXCEPTION 'organizations.currency cannot be changed after creation (see migration 000029)';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS organizations_currency_immutable ON organizations;
CREATE TRIGGER organizations_currency_immutable
    BEFORE UPDATE ON organizations
    FOR EACH ROW EXECUTE FUNCTION reject_organization_currency_change();

COMMIT;
