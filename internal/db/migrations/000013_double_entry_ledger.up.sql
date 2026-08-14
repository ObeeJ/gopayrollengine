-- 000013_double_entry_ledger.up.sql
-- A real double-entry ledger. Until now the system tracked money as mutable
-- amounts on domain rows (payrolls.total_amount, payroll_items.amount) with no
-- independent record of value movement. That shape cannot answer the only
-- question that matters during an incident: "where did the money go, and does
-- it still add up?"
--
-- Three entities, the classic shape:
--   ledger_accounts      — buckets of value, each with a normal balance
--   ledger_transactions  — the atomic grouping; entries only exist inside one
--   ledger_entries       — individual debit/credit lines, append-only
--
-- Balances are NEVER stored. They are derived by summing entries. A stored
-- balance is a cache that silently drifts; a derived balance cannot lie.
--
-- The balanced-transaction invariant is enforced by a DEFERRABLE constraint
-- trigger, not by application code. Application-level checks hold only while
-- every future writer remembers them; a deferred constraint fires at COMMIT for
-- every writer, including psql sessions and future services.

BEGIN;

-- Accounts --------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ledger_accounts (
    id              TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id),
    -- NULL for org-level accounts (funding pool, fee income); set for
    -- per-worker accounts (advance receivable).
    employee_id     TEXT REFERENCES employees(id),
    account_type    TEXT NOT NULL,
    normal_balance  TEXT NOT NULL CHECK (normal_balance IN ('debit', 'credit')),
    currency        TEXT NOT NULL DEFAULT 'NGN',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One account per (org, employee, type). COALESCE lets the unique index cover
-- org-level accounts, where employee_id IS NULL and would otherwise never collide.
CREATE UNIQUE INDEX IF NOT EXISTS idx_ledger_accounts_identity
    ON ledger_accounts (organization_id, COALESCE(employee_id, ''), account_type);

-- Transactions ----------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ledger_transactions (
    id              TEXT PRIMARY KEY,
    organization_id TEXT NOT NULL REFERENCES organizations(id),
    kind            TEXT NOT NULL,
    reference       TEXT,
    -- Every money-moving caller supplies one. The unique index below is what
    -- makes a retried disbursement a no-op instead of a double-post.
    idempotency_key TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_ledger_tx_idempotency
    ON ledger_transactions (organization_id, idempotency_key);

CREATE INDEX IF NOT EXISTS idx_ledger_tx_reference
    ON ledger_transactions (organization_id, reference);

-- Entries ---------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS ledger_entries (
    id              BIGSERIAL PRIMARY KEY,
    transaction_id  TEXT NOT NULL REFERENCES ledger_transactions(id),
    account_id      TEXT NOT NULL REFERENCES ledger_accounts(id),
    organization_id TEXT NOT NULL REFERENCES organizations(id),
    direction       TEXT NOT NULL CHECK (direction IN ('debit', 'credit')),
    -- Strictly positive: direction carries the sign. Allowing negative amounts
    -- would let a single entry masquerade as its own opposite.
    amount_kobo     BIGINT NOT NULL CHECK (amount_kobo > 0),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_ledger_entries_account ON ledger_entries (account_id);
CREATE INDEX IF NOT EXISTS idx_ledger_entries_tx      ON ledger_entries (transaction_id);

-- Invariant 1: entries are append-only ----------------------------------------
-- Corrections are new reversing entries, never edits. This is what makes the
-- ledger admissible as evidence.
CREATE OR REPLACE FUNCTION ledger_entries_append_only() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'ledger_entries is append-only: % is not permitted', TG_OP;
END;
$$;

DROP TRIGGER IF EXISTS trg_ledger_entries_append_only ON ledger_entries;
CREATE TRIGGER trg_ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON ledger_entries
    FOR EACH ROW EXECUTE FUNCTION ledger_entries_append_only();

-- Invariant 2: every transaction balances -------------------------------------
-- DEFERRABLE INITIALLY DEFERRED so the check runs at COMMIT, after all of a
-- transaction's entries are inserted. A non-deferred trigger would reject the
-- very first entry, since one line can never balance on its own.
CREATE OR REPLACE FUNCTION ledger_transaction_balanced() RETURNS TRIGGER
LANGUAGE plpgsql AS $$
DECLARE
    debit_total  BIGINT;
    credit_total BIGINT;
BEGIN
    SELECT
        COALESCE(SUM(amount_kobo) FILTER (WHERE direction = 'debit'),  0),
        COALESCE(SUM(amount_kobo) FILTER (WHERE direction = 'credit'), 0)
      INTO debit_total, credit_total
      FROM ledger_entries
     WHERE transaction_id = NEW.transaction_id;

    IF debit_total <> credit_total THEN
        RAISE EXCEPTION
            'ledger transaction % is unbalanced: debits=% credits=%',
            NEW.transaction_id, debit_total, credit_total;
    END IF;

    RETURN NULL;
END;
$$;

DROP TRIGGER IF EXISTS trg_ledger_entries_balanced ON ledger_entries;
CREATE CONSTRAINT TRIGGER trg_ledger_entries_balanced
    AFTER INSERT ON ledger_entries
    DEFERRABLE INITIALLY DEFERRED
    FOR EACH ROW EXECUTE FUNCTION ledger_transaction_balanced();

-- Tenant isolation ------------------------------------------------------------
ALTER TABLE ledger_accounts     ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_accounts     FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ledger_accounts_org_isolation ON ledger_accounts;
CREATE POLICY ledger_accounts_org_isolation ON ledger_accounts
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

ALTER TABLE ledger_transactions ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_transactions FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ledger_transactions_org_isolation ON ledger_transactions;
CREATE POLICY ledger_transactions_org_isolation ON ledger_transactions
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

ALTER TABLE ledger_entries      ENABLE ROW LEVEL SECURITY;
ALTER TABLE ledger_entries      FORCE  ROW LEVEL SECURITY;
DROP POLICY IF EXISTS ledger_entries_org_isolation ON ledger_entries;
CREATE POLICY ledger_entries_org_isolation ON ledger_entries
    USING (organization_id = app_current_org_id())
    WITH CHECK (organization_id = app_current_org_id());

COMMIT;
