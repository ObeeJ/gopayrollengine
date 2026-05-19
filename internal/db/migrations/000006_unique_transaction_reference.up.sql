-- 000006_unique_transaction_reference.up.sql
-- Defence-in-depth against duplicate webhook ingestion. The application path
-- already rejects duplicates via the FSM CAS in models.TransitionStatus, but
-- a structural DB constraint guarantees the invariant even if a future code
-- path writes transaction_reference outside the FSM (e.g. a reconciliation
-- script, a hot-fix patch, a different ORM).
--
-- The index is partial so existing rows with NULL/empty transaction_reference
-- don't collide on each other — only meaningful (assigned) references must
-- be unique. Backfill of historical refs is left to the application layer.

CREATE UNIQUE INDEX IF NOT EXISTS idx_payroll_items_txref_unique
    ON payroll_items (transaction_reference)
    WHERE transaction_reference IS NOT NULL AND transaction_reference <> '';
