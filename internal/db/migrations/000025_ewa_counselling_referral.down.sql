-- 000025_ewa_counselling_referral.down.sql
BEGIN;
ALTER TABLE ewa_policies DROP COLUMN counselling_resource_name;
ALTER TABLE ewa_policies DROP COLUMN counselling_contact;
COMMIT;
