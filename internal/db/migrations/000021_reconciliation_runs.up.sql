-- 000021_reconciliation_runs.up.sql
-- Phase 3: "Reconciliation job: ledger balances vs. Monnify statements,
-- alerting on drift." See internal/services/reconciliation_job.go for the
-- reasoning behind what gets compared and why.
--
-- System-level, not tenant-scoped — no organization_id, no RLS. There is one
-- shared Monnify disbursement wallet (MONNIFY_SOURCE_WALLET) across every
-- org, so reconciliation is inherently a platform-level concern: an
-- individual org's cash_settlement balance is an accounting allocation of
-- that shared pool, not something with its own real-world bank balance to
-- check against.

BEGIN;

CREATE TABLE IF NOT EXISTS reconciliation_runs (
    id                          BIGSERIAL PRIMARY KEY,
    run_at                      TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- The actual Monnify source wallet balance at run time.
    wallet_balance_kobo         BIGINT NOT NULL,

    -- Sum of every org's cash_settlement ledger balance (Credit-normal: net
    -- advances disbursed minus net employer deposits) at run time. Positive
    -- means the platform is carrying more uncovered exposure than it has
    -- collected back.
    total_cash_settlement_kobo  BIGINT NOT NULL,

    -- Computed against the immediately preceding run: how much the wallet
    -- balance moved, plus how much total_cash_settlement moved, since last
    -- time. In a fully reconciled system these offset exactly (the wallet
    -- drains by precisely however much uncovered exposure grew) and this is
    -- zero. NULL on an org's first-ever run, when there is no prior run to
    -- diff against.
    drift_kobo                  BIGINT,
    alerted                     BOOLEAN NOT NULL DEFAULT FALSE,

    created_at                  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_reconciliation_runs_run_at ON reconciliation_runs (run_at DESC);

COMMIT;
