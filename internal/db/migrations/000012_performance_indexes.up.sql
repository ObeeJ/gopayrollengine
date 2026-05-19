-- Composite index on audit_events — compliance report's 30-day range scan degrades without it.
CREATE INDEX IF NOT EXISTS idx_audit_events_org_created
    ON audit_events (organization_id, created_at DESC);

-- Composite index on payroll_items — reconciliation COUNT(*) WHERE status='failed' does a full scan otherwise.
CREATE INDEX IF NOT EXISTS idx_payroll_items_payroll_status
    ON payroll_items (payroll_id, status);
