-- 000015_ledger_multi_currency.up.sql
-- Makes currency a first-class, database-enforced dimension of the ledger.
--
-- THE BUG THIS CLOSES
-- ledger_accounts carried a `currency` column from 000013, but ledger_entries
-- did not. The balance trigger summed raw amount_kobo across every entry in a
-- transaction, so a ₦50,000 debit and a $50,000 credit balanced perfectly. The
-- moment a second currency exists, that is silent money corruption with a
-- passing test suite — the worst possible failure mode for a ledger.
--
-- THREE INVARIANTS, ALL ENFORCED IN THE DATABASE
--   1. Every entry names its currency.
--   2. An entry's currency must equal its account's currency. An account is a
--      bucket of one currency; mixing them makes its balance meaningless.
--   3. A transaction is single-currency, and balances within that currency.
--
-- WHY SINGLE-CURRENCY TRANSACTIONS
-- Cross-currency movement is not one transaction, it is two: money leaves a
-- currency and separately arrives in another, joined by an FX rate that has a
-- timestamp and a spread. Modelling FX as one "balanced" multi-currency
-- transaction requires the rate to be embedded in the balance check, which
-- makes the invariant depend on a market price. Instead, FX is two
-- single-currency transactions against per-currency FX clearing accounts, and
-- the spread is an explicit posting rather than a rounding artefact.
--
-- The amount column keeps the name `amount_kobo` for now. Renaming it is a
-- separate mechanical change; doing it here would bury this invariant inside a
-- large diff. It holds minor units of the row's own currency.

BEGIN;

-- 1 — currency on entries ----------------------------------------------------
ALTER TABLE ledger_entries
    ADD COLUMN IF NOT EXISTS currency TEXT NOT NULL DEFAULT 'NGN';

-- Existing rows predate multi-currency and are NGN by construction: the only
-- writers were the EWA advance and settlement paths, both NGN-only.
UPDATE ledger_entries SET currency = 'NGN' WHERE currency IS NULL;

-- Drop the default once backfilled — new writers must state the currency
-- explicitly rather than silently inheriting NGN.
ALTER TABLE ledger_entries ALTER COLUMN currency DROP DEFAULT;

ALTER TABLE ledger_accounts ALTER COLUMN currency SET NOT NULL;

-- Restrict both tables to the currencies the platform can actually settle. A
-- balance in a currency no configured PSP pays out is a balance nobody can
-- withdraw.
ALTER TABLE ledger_entries  DROP CONSTRAINT IF EXISTS ledger_entries_currency_supported;
ALTER TABLE ledger_entries  ADD CONSTRAINT ledger_entries_currency_supported
    CHECK (currency IN ('NGN','USD','GHS','KES','ZAR','XOF','GBP','EUR'));

ALTER TABLE ledger_accounts DROP CONSTRAINT IF EXISTS ledger_accounts_currency_supported;
ALTER TABLE ledger_accounts ADD CONSTRAINT ledger_accounts_currency_supported
    CHECK (currency IN ('NGN','USD','GHS','KES','ZAR','XOF','GBP','EUR'));

CREATE INDEX IF NOT EXISTS idx_ledger_entries_account_currency
    ON ledger_entries (account_id, currency);

-- 1b — currency belongs in the account's identity ----------------------------
-- The 000013 index was UNIQUE on (org, employee, account_type) with no currency
-- component, so an organisation could hold exactly ONE receivable account no
-- matter how many currencies it dealt in. Creating the USD counterpart of an
-- existing NGN account failed on a duplicate key. Currency is part of what
-- makes an account distinct, not an attribute of it.
DROP INDEX IF EXISTS idx_ledger_accounts_identity;
CREATE UNIQUE INDEX idx_ledger_accounts_identity
    ON ledger_accounts (organization_id, COALESCE(employee_id, ''), account_type, currency);

-- 2 — an entry must match its account's currency -----------------------------
CREATE OR REPLACE FUNCTION ledger_entry_currency_matches_account() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    account_currency TEXT;
BEGIN
    SELECT currency INTO account_currency
      FROM ledger_accounts
     WHERE id = NEW.account_id;

    IF account_currency IS NULL THEN
        RAISE EXCEPTION 'ledger entry references unknown account %', NEW.account_id;
    END IF;

    IF account_currency <> NEW.currency THEN
        RAISE EXCEPTION
            'ledger entry currency % does not match account % currency %',
            NEW.currency, NEW.account_id, account_currency;
    END IF;

    RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_ledger_entry_currency_matches_account ON ledger_entries;
CREATE TRIGGER trg_ledger_entry_currency_matches_account
    BEFORE INSERT ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entry_currency_matches_account();

-- 3 — transactions are single-currency and balance within it -----------------
-- Replaces the 000013 balance trigger. Deferred to COMMIT for the same reason
-- as before: a single entry can never balance on its own.
CREATE OR REPLACE FUNCTION ledger_transaction_balanced() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    currency_count INTEGER;
    tx_currency    TEXT;
    debit_total    BIGINT;
    credit_total   BIGINT;
BEGIN
    SELECT COUNT(DISTINCT currency) INTO currency_count
      FROM ledger_entries
     WHERE transaction_id = NEW.transaction_id;

    IF currency_count > 1 THEN
        RAISE EXCEPTION
            'ledger transaction % spans % currencies; cross-currency movement must be posted as separate single-currency transactions through an FX clearing account',
            NEW.transaction_id, currency_count;
    END IF;

    SELECT
        MIN(currency),
        COALESCE(SUM(amount_kobo) FILTER (WHERE direction = 'debit'),  0),
        COALESCE(SUM(amount_kobo) FILTER (WHERE direction = 'credit'), 0)
      INTO tx_currency, debit_total, credit_total
      FROM ledger_entries
     WHERE transaction_id = NEW.transaction_id;

    IF debit_total <> credit_total THEN
        RAISE EXCEPTION
            'ledger transaction % is unbalanced in %: debits=% credits=%',
            NEW.transaction_id, tx_currency, debit_total, credit_total;
    END IF;

    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS trg_ledger_entries_balanced ON ledger_entries;
CREATE CONSTRAINT TRIGGER trg_ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_transaction_balanced();

-- 4 — FX clearing account type ------------------------------------------------
-- One per (org, currency). An FX conversion debits the source currency's
-- clearing account and credits the destination's, in two separate balanced
-- transactions. The pair nets to the spread, which is then a visible number
-- rather than an unexplained discrepancy.
COMMENT ON TABLE ledger_accounts IS
    'Buckets of value. One currency each. Balances are derived by summing ledger_entries, never stored.';

COMMIT;
