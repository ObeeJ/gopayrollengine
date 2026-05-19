-- 000011_payroll_item_webhook_lookup.up.sql
-- Closes the last RLS gap: payroll_items kept the permissive bypass in 000010
-- because the Monnify webhook handler needs to load an item by UUID *before*
-- it knows which org owns it. We solve that with a SECURITY DEFINER function
-- whose blast radius is exactly one row — then drop the bypass on the table
-- so every other code path on payroll_items must go through WithOrgScope.
--
-- SECURITY DEFINER runs the function with the privileges of its owner (the
-- migration role, typically postgres) regardless of the caller. That role
-- bypasses RLS via ownership, so this is the only legitimate path for an
-- unscoped read. SET search_path is mandatory to prevent function-call
-- injection through schema shadowing.

BEGIN;

CREATE OR REPLACE FUNCTION lookup_payroll_item_for_webhook(p_ref TEXT)
RETURNS SETOF payroll_items
LANGUAGE sql
SECURITY DEFINER
STABLE
SET search_path = public, pg_temp
AS $$
    SELECT * FROM payroll_items WHERE id = p_ref LIMIT 1;
$$;

-- Allow the app role to call it. PUBLIC keeps it simple in dev where the
-- application connects as postgres; tighten to a dedicated role in prod.
GRANT EXECUTE ON FUNCTION lookup_payroll_item_for_webhook(TEXT) TO PUBLIC;

-- Now drop the bypass clause on payroll_items — webhook reads go through the
-- function above, every other read/write goes through WithOrgScope.
DROP POLICY IF EXISTS payroll_items_org_isolation ON payroll_items;
CREATE POLICY payroll_items_org_isolation ON payroll_items
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

COMMIT;
