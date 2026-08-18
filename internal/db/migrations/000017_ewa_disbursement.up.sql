-- 000017_ewa_disbursement.up.sql
-- Wires EWA advances to an actual payment rail. Until now RequestAdvance
-- posted a ledger entry and stopped — the ledger recorded money leaving that
-- never actually left. This closes that gap: an approved advance is submitted
-- to a provider, and the provider's webhook confirms or reverses it.
--
-- provider_name / provider_reference record which rail handled a given advance
-- and that rail's own identifier for the transfer, needed to correlate a
-- webhook or a status poll back to this row.
--
-- The webhook lookup problem is the same one payroll_items solved in
-- migrations 000008-000011: the callback carries a transfer reference but not
-- an org_id, so the row must be found before RLS has anything to scope by.
-- Payroll got there in three migrations because RLS was retrofitted onto a
-- table that predated it. ewa_advances has never had a permissive bypass — it
-- goes straight to the SECURITY DEFINER function payroll_items eventually
-- landed on, with the same one-row blast radius and the same justification:
-- the request is authenticated by HMAC before any DB access, and every write
-- after the lookup runs inside WithOrgScope using the org_id the lookup itself
-- revealed.

BEGIN;

ALTER TABLE ewa_advances
    ADD COLUMN IF NOT EXISTS provider_name      TEXT,
    ADD COLUMN IF NOT EXISTS provider_reference TEXT;

-- A provider reference is unique per provider (Monnify and Paystack both mint
-- their own reference namespace), so the pair must be unique, not the
-- reference alone — two different providers could coincidentally reuse the
-- same string.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ewa_advances_provider_reference
    ON ewa_advances (provider_name, provider_reference)
    WHERE provider_reference IS NOT NULL;

CREATE OR REPLACE FUNCTION lookup_ewa_advance_for_webhook(p_ref TEXT)
RETURNS SETOF ewa_advances
LANGUAGE sql
SECURITY DEFINER
STABLE
SET search_path = public, pg_temp
AS $$
    SELECT * FROM ewa_advances WHERE id = p_ref LIMIT 1;
$$;

GRANT EXECUTE ON FUNCTION lookup_ewa_advance_for_webhook(TEXT) TO PUBLIC;

COMMIT;
