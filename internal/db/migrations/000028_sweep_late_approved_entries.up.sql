-- 000028_sweep_late_approved_entries.up.sql
-- Phase 5 roadmap item: "sweep late-approved timesheet entries into a later
-- payroll run."
--
-- WHY THIS EXISTS
-- payroll_service.go pays hourly employees for entries approved by the time
-- a run happens. An entry approved AFTER its own period's run has already
-- gone out was, until now, gone: the next period's run only ever looked at
-- that next period's own work_date range, so the late entry's minutes were
-- never paid by any run — not a display bug, a wages-never-paid bug.
--
-- The fix (see services.ComputeHourlyGross) is to track, per entry, which
-- payroll item actually paid it. A run then sweeps every approved entry that
-- has never been paid, regardless of which period its work_date falls in —
-- not just the entries dated within the period being run.
--
-- paid_payroll_item_id has no NOT NULL / CHECK: most entries stay NULL
-- ("unpaid") for a while, then get stamped once, atomically, in the same
-- transaction that creates the payroll item — see
-- models.MarkTimeEntriesPaid. It is never cleared or reassigned: correcting
-- a wrong payment is a new reversing transaction elsewhere, not editing this
-- column, the same append-only posture the ledger itself takes.

BEGIN;

ALTER TABLE time_entries
    ADD COLUMN IF NOT EXISTS paid_payroll_item_id TEXT REFERENCES payroll_items(id);

-- Drives the sweep query: "every approved, unpaid entry for this employee up
-- to some date" — the payroll hot path, run once per employee per payroll.
CREATE INDEX IF NOT EXISTS idx_time_entries_unpaid_approved
    ON time_entries (organization_id, employee_id, status, work_date)
    WHERE deleted_at IS NULL AND paid_payroll_item_id IS NULL;

COMMIT;
