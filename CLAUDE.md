# CLAUDE.md

Architecture notes for future Claude sessions. Code is the source of truth; this file captures the non-obvious invariants.

## Money

Every monetary value is `money.Kobo` (int64 minor units). Never reintroduce float64 for money. `pkg/money/money.go` has `Value()`/`Scan()` for GORM BIGINT persistence and `Naira()`/`FromNaira()` for display + the Monnify wire boundary only. Use `money.Sum([]Kobo)` not `+=` — `Sum` is overflow-checked.

## Tenant isolation: RLS, not WHERE clauses

Every tenant-scoped DB write or read must run inside `models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error { ... })`. The helper opens a transaction and sets `app.org_id` via `set_config(..., true)`; Postgres RLS policies (migrations 000008–000011) then filter rows server-side. A forgotten WHERE clause returns zero rows, not another tenant's data.

- Don't write raw `models.DB.Where("organization_id = ?", orgID)...` queries. Use `WithOrgScope` + `repo.WithTx(tx)`.
- The only legitimate unscoped read is the webhook's `lookup_payroll_item_for_webhook(ref)` call (SECURITY DEFINER function, migration 000011). Reason: Monnify callbacks don't carry orgID, so we need a single-UUID read to learn it. All subsequent writes are scoped via the loaded `item.OrganizationID`.
- The `audit_events` policy allows `organization_id IS NULL` for system-level events. Pass `""` to `AppendAuditTx` for those.

## FSM transitions and atomicity

`models.TransitionStatus(tx, row, current, next)` is a CAS UPDATE. If `RowsAffected == 0` it returns `models.ErrStaleStatus` — treat that as idempotent success in concurrent paths (webhook), or as a duplicate-task abort (worker).

Counter init and FSM transition for a payroll happen as one atomic UPDATE in the worker (`UPDATE payrolls SET status=processing, pending_count=N WHERE id=? AND status IN (pending, failed)`). Never split that into separate statements — webhooks racing during the Monnify call depend on `pending_count` already being correct.

The webhook decrement uses `UPDATE ... RETURNING pending_count, status` so exactly one path observes zero (no decrement-then-SELECT TOCTOU).

## Encryption

Two keys, both base64-encoded 32 bytes:
- `ENCRYPTION_KEK` — AES-256-GCM key for PII columns (email, account_number, bank_code).
- `ENCRYPTION_HMAC_KEY` — HMAC-SHA256 key for the email blind index (so encrypted email is still searchable + uniquely constrained per org).

`EncryptedString.MarshalJSON` masks to `****<last4>`. Don't log the decrypted form.

## Webhook handler

1. HMAC-SHA512 verification — reject forged requests.
2. Bloom filter check — O(1) duplicate skip before any DB.
3. UUID lookup via `lookup_payroll_item_for_webhook` (RLS bypass, single row).
4. All writes (status transition, decrement, reconcile, audit) inside one `WithOrgScope` tx with `orgID = item.OrganizationID`.
5. Bloom filter add — best-effort, after success.

A `ErrStaleStatus` on the item transition means a concurrent webhook won; return 200 OK without further work.

## Tests

Unit tests run with `make test`. Integration tests (`//go:build integration`) need a real Postgres + miniredis and run with `make test-integration`. The integration suite proves: RLS blocks cross-tenant reads/writes, the webhook reconciles exactly once under 25 concurrent fires, the worker sets `pending_count` before Monnify, Kobo round-trips through BIGINT cleanly.

CI coverage gate is 30% (`.github/workflows/ci.yml`). It's deliberately loose because services + repository code is mostly exercised by integration tests that don't count toward in-package coverage. Raise as dedicated unit tests land.

## Migrations

Versioned SQL in `internal/db/migrations`. The app runs them on `InitDB`; the test workflow applies them via the `migrate/migrate` Docker image (`make migrate-up`). Don't AutoMigrate. Don't edit applied migrations — add a new one.

Current state through 000011:
- `000005_money_kobo` — NUMERIC → BIGINT with CHECK ≥ 0
- `000006_unique_transaction_reference` — partial unique index on item ID
- `000007_email_encryption_and_per_org` — `email_hmac` column + per-org unique
- `000008_row_level_security` — RLS on employees/payrolls/payroll_items/advance_requests/audit_events (permissive bypass while migrating)
- `000009_rls_consent_bvn` — RLS on consent_records + bvn_verifications
- `000010_rls_strict` — drop permissive bypass on 6 tables (keeps it on payroll_items)
- `000011_payroll_item_webhook_lookup` — SECURITY DEFINER lookup function + drop the last bypass

## Roles

The `payroll_app` role is `NOSUPERUSER` and is the production application role — RLS policies bite it. Don't grant it `BYPASSRLS`. The migrations are owned by `postgres` (superuser), which is why the SECURITY DEFINER function can read past RLS.
