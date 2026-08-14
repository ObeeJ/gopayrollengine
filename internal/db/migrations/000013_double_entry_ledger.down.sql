-- 000013_double_entry_ledger.down.sql
BEGIN;

DROP TRIGGER IF EXISTS trg_ledger_entries_balanced    ON ledger_entries;
DROP TRIGGER IF EXISTS trg_ledger_entries_append_only ON ledger_entries;
DROP FUNCTION IF EXISTS ledger_transaction_balanced();
DROP FUNCTION IF EXISTS ledger_entries_append_only();

DROP TABLE IF EXISTS ledger_entries;
DROP TABLE IF EXISTS ledger_transactions;
DROP TABLE IF EXISTS ledger_accounts;

COMMIT;
