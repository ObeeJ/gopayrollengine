-- 000031_d2c_debit_collections.up.sql
-- Phase 5 roadmap item: direct-to-consumer EWA, part 2 — the debit-mandate
-- and collection-attempt tables underneath the settlement path.
--
-- WHY THIS EXISTS
-- Migration 000030 gave a D2C worker a way to LINK a bank account for
-- reading — payday prediction. This migration adds the separate, explicit
-- step of authorizing that account to be DEBITED, and a table to track each
-- attempt to actually pull money from it. Consent to read and consent to be
-- debited are deliberately two different columns set at two different
-- times, not one: a worker who links an account so eligibility can be
-- computed has not thereby agreed that money may be pulled from it. See
-- internal/integrations/banklink's DebitProvider doc comment for the same
-- split at the Go interface level.
--
-- d2c_debit_collections is one row per ATTEMPT, not per advance — an
-- attempt can fail (insufficient funds on the predicted day) and be retried
-- on a later predicted payday, and the full history of attempts is exactly
-- the kind of audit trail this codebase keeps everywhere else money moves
-- (ledger_entries, ewa_accrual_snapshots). A successful collection is
-- recorded here AND drives the same Dr cash_settlement / Cr
-- advance_receivable ledger posting SettleAdvancesForPayrollItem makes on
-- the payroll-funded path — see the (forthcoming) service code, not this
-- migration, for that.
--
-- lookup_d2c_collection_for_webhook mirrors lookup_payroll_item_for_webhook
-- (migration 000011) and lookup_ewa_advance_for_webhook (000017) exactly:
-- SECURITY DEFINER, blast radius one row, the only legitimate unscoped read
-- on this table. A debit-provider webhook confirming a collection doesn't
-- carry our org_id, so this is how the handler learns it before it can
-- open a properly org-scoped transaction to do anything else.

BEGIN;

ALTER TABLE d2c_bank_links
    ADD COLUMN IF NOT EXISTS debit_mandate_ref TEXT,
    ADD COLUMN IF NOT EXISTS debit_authorized_at TIMESTAMPTZ;

CREATE TABLE IF NOT EXISTS d2c_debit_collections (
    id               TEXT PRIMARY KEY,
    organization_id  TEXT NOT NULL REFERENCES organizations(id),
    employee_id      TEXT NOT NULL REFERENCES employees(id),
    advance_id       TEXT NOT NULL REFERENCES ewa_advances(id),

    amount_kobo      BIGINT NOT NULL CHECK (amount_kobo > 0),
    attempt_number   INTEGER NOT NULL CHECK (attempt_number > 0),
    scheduled_for    DATE NOT NULL,               -- the predicted payday this attempt targets

    status           TEXT NOT NULL DEFAULT 'pending'
                      CHECK (status IN ('pending', 'successful', 'failed')),
    provider         TEXT NOT NULL,
    provider_reference TEXT,

    attempted_at     TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Drives "has this advance already been attempted for its current predicted
-- payday" and the retry-attempt-numbering query alike.
CREATE INDEX IF NOT EXISTS idx_d2c_debit_collections_advance
    ON d2c_debit_collections (organization_id, advance_id, attempt_number DESC);

-- At most one attempt in flight per advance at a time — a second collection
-- sweep must not fire a duplicate debit while an earlier attempt is still
-- pending confirmation.
CREATE UNIQUE INDEX IF NOT EXISTS idx_d2c_debit_collections_one_pending
    ON d2c_debit_collections (organization_id, advance_id)
    WHERE status = 'pending';

-- A retried submission against the same provider reference must be a no-op,
-- the same idempotency guarantee every ledger posting in this codebase has.
CREATE UNIQUE INDEX IF NOT EXISTS idx_d2c_debit_collections_provider_ref
    ON d2c_debit_collections (provider_reference)
    WHERE provider_reference IS NOT NULL;

ALTER TABLE d2c_debit_collections ENABLE ROW LEVEL SECURITY;
ALTER TABLE d2c_debit_collections FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS d2c_debit_collections_org_isolation ON d2c_debit_collections;
CREATE POLICY d2c_debit_collections_org_isolation ON d2c_debit_collections
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

CREATE OR REPLACE FUNCTION lookup_d2c_collection_for_webhook(p_ref TEXT)
RETURNS SETOF d2c_debit_collections
LANGUAGE sql
SECURITY DEFINER
STABLE
SET search_path = public, pg_temp
AS $$
    SELECT * FROM d2c_debit_collections WHERE provider_reference = p_ref LIMIT 1;
$$;

GRANT EXECUTE ON FUNCTION lookup_d2c_collection_for_webhook(TEXT) TO PUBLIC;

COMMIT;
