-- 000029_organization_currency.down.sql
BEGIN;
DROP TRIGGER IF EXISTS organizations_currency_immutable ON organizations;
DROP FUNCTION IF EXISTS reject_organization_currency_change();
ALTER TABLE organizations DROP COLUMN IF EXISTS currency;
COMMIT;
