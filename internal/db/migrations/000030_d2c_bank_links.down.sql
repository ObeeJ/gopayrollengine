-- 000030_d2c_bank_links.down.sql
BEGIN;
DROP TABLE IF EXISTS d2c_bank_links;
ALTER TABLE organizations DROP COLUMN IF EXISTS is_d2c;
COMMIT;
