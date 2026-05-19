-- 000011_payroll_item_webhook_lookup.down.sql
-- Restores the permissive bypass on payroll_items and drops the lookup function.

BEGIN;

DROP POLICY IF EXISTS payroll_items_org_isolation ON payroll_items;
CREATE POLICY payroll_items_org_isolation ON payroll_items
    USING (app_current_org_id() = '' OR organization_id = app_current_org_id())
    WITH CHECK (app_current_org_id() = '' OR organization_id = app_current_org_id());

DROP FUNCTION IF EXISTS lookup_payroll_item_for_webhook(TEXT);

COMMIT;
