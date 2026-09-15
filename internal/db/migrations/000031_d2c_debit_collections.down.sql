-- 000031_d2c_debit_collections.down.sql
BEGIN;
DROP FUNCTION IF EXISTS lookup_d2c_collection_for_webhook(TEXT);
DROP TABLE IF EXISTS d2c_debit_collections;
ALTER TABLE d2c_bank_links
    DROP COLUMN IF EXISTS debit_mandate_ref,
    DROP COLUMN IF EXISTS debit_authorized_at;
COMMIT;
