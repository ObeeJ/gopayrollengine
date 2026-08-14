-- 000015_ledger_multi_currency.down.sql
-- Restores the 000013 single-currency balance trigger.
BEGIN;

DROP TRIGGER IF EXISTS trg_ledger_entry_currency_matches_account ON ledger_entries;
DROP FUNCTION IF EXISTS ledger_entry_currency_matches_account();

ALTER TABLE ledger_entries  DROP CONSTRAINT IF EXISTS ledger_entries_currency_supported;
ALTER TABLE ledger_accounts DROP CONSTRAINT IF EXISTS ledger_accounts_currency_supported;
DROP INDEX IF EXISTS idx_ledger_entries_account_currency;
ALTER TABLE ledger_entries  DROP COLUMN IF EXISTS currency;

CREATE OR REPLACE FUNCTION ledger_transaction_balanced() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    debit_total  BIGINT;
    credit_total BIGINT;
BEGIN
    SELECT
        COALESCE(SUM(amount_kobo) FILTER (WHERE direction = 'debit'),  0),
        COALESCE(SUM(amount_kobo) FILTER (WHERE direction = 'credit'), 0)
      INTO debit_total, credit_total
      FROM ledger_entries
     WHERE transaction_id = NEW.transaction_id;

    IF debit_total <> credit_total THEN
        RAISE EXCEPTION
            'ledger transaction % is unbalanced: debits=% credits=%',
            NEW.transaction_id, debit_total, credit_total;
    END IF;

    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS trg_ledger_entries_balanced ON ledger_entries;
CREATE CONSTRAINT TRIGGER trg_ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_transaction_balanced();

COMMIT;
