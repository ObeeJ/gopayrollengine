-- 000025_ewa_counselling_referral.up.sql
-- Phase 4 roadmap item: "Referral to financial counselling at the strained
-- tier."
--
-- WHY THIS EXISTS
-- The dependency score identifies people the product cannot help by giving
-- them more of the same. At the Strained tier, the emergency floor already
-- keeps a minimum available — but the honest next step for a worker in that
-- position often isn't another advance at all. This is deliberately a
-- referral to the EMPLOYER's own resource, not a third-party service this
-- product picks on its behalf: an employer's existing Employee Assistance
-- Program (or equivalent) is who actually has standing to make that offer,
-- and no default name or contact is ever invented here. Unconfigured, the
-- eligibility response still names the option, just without specifics.

BEGIN;

ALTER TABLE ewa_policies ADD COLUMN counselling_resource_name TEXT NOT NULL DEFAULT '';
ALTER TABLE ewa_policies ADD COLUMN counselling_contact       TEXT NOT NULL DEFAULT '';

COMMIT;
