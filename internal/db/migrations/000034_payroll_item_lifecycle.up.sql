-- 000034_payroll_item_lifecycle.up.sql
-- Payroll item lifecycle: retrying a failed payout, proving what was paid, and
-- recording how a stuck item was resolved. Expand-only: every column is
-- nullable or defaulted, so the previous image keeps working against it.
--
-- attempt: a retried item must go to the bank under a NEW reference. A
-- provider that treats the reference as an idempotency key can answer a retry
-- of a failed reference with the failed result, so the retry would never run.
-- Attempt 1 keeps the bare item ID (backward compatible with every existing
-- row and webhook); attempt N>1 is sent as "<item id>-R<N>". A callback whose
-- attempt does not match the item's current attempt is stale and is ignored,
-- so attempt 1's late FAILED cannot fail attempt 2's in-flight payout.
--
-- gross/advances/savings: the breakdown behind amount (net), stored at
-- creation so a payslip can be shown without reverse-engineering it. NULL on
-- rows created before this migration.
--
-- sent_at / settled_at: when the item was handed to the bank and when it was
-- confirmed. sent_at drives stuck-item detection; settled_at is the payslip's
-- "paid on" date.
--
-- resolved_by / resolution_note / resolution_evidence: a payout whose callback
-- never arrived can be resolved by an admin checking the provider dashboard.
-- The attestation is recorded on the row (and in the audit log).

BEGIN;

ALTER TABLE payroll_items
    ADD COLUMN IF NOT EXISTS attempt                INT         NOT NULL DEFAULT 1,
    ADD COLUMN IF NOT EXISTS gross_kobo             BIGINT,
    ADD COLUMN IF NOT EXISTS advances_deducted_kobo BIGINT,
    ADD COLUMN IF NOT EXISTS savings_kobo           BIGINT,
    ADD COLUMN IF NOT EXISTS sent_at                TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS settled_at             TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS resolved_by            TEXT,
    ADD COLUMN IF NOT EXISTS resolution_note        TEXT,
    ADD COLUMN IF NOT EXISTS resolution_evidence    TEXT;

-- NOT VALID first: adding a CHECK normally scans the whole table under an
-- exclusive lock. NOT VALID takes the lock only briefly and enforces the rule
-- for new writes; VALIDATE then scans under a lock that doesn't block writes.
-- Guarded so the file can be re-run after a half-applied attempt (the two
-- transactions below can't be atomic together): ADD CONSTRAINT has no
-- IF NOT EXISTS.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'payroll_items_attempt_positive') THEN
        ALTER TABLE payroll_items
            ADD CONSTRAINT payroll_items_attempt_positive CHECK (attempt >= 1) NOT VALID;
    END IF;
    IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'payroll_items_breakdown_nonnegative') THEN
        ALTER TABLE payroll_items
            ADD CONSTRAINT payroll_items_breakdown_nonnegative CHECK (
                (gross_kobo IS NULL OR gross_kobo >= 0)
                AND (advances_deducted_kobo IS NULL OR advances_deducted_kobo >= 0)
                AND (savings_kobo IS NULL OR savings_kobo >= 0)) NOT VALID;
    END IF;
END
$$;

COMMIT;

BEGIN;
ALTER TABLE payroll_items VALIDATE CONSTRAINT payroll_items_attempt_positive;
ALTER TABLE payroll_items VALIDATE CONSTRAINT payroll_items_breakdown_nonnegative;

-- "Items still processing, oldest first" — the stuck-payout scan filters on
-- COALESCE(sent_at, updated_at) (sent_at is NULL on rows marked processing
-- before this migration), so the index is on that same expression.
CREATE INDEX IF NOT EXISTS idx_payroll_items_processing
    ON payroll_items ((COALESCE(sent_at, updated_at))) WHERE status = 'processing';

-- The payslip list: one worker's items, newest first.
CREATE INDEX IF NOT EXISTS idx_payroll_items_employee_recent
    ON payroll_items (organization_id, employee_id, created_at DESC);

COMMIT;
