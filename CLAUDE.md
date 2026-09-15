# CLAUDE.md

Architecture notes for future Claude sessions. Code is the source of truth; this file captures the non-obvious invariants.

## Money

Every monetary value is `money.Kobo` (int64 minor units). Never reintroduce float64 for money. `pkg/money/money.go` has `Value()`/`Scan()` for GORM BIGINT persistence and `Naira()`/`FromNaira()` for display + the Monnify wire boundary only. Use `money.Sum([]Kobo)` not `+=` — `Sum` is overflow-checked.

## Currency: Kobo fields are currency-less; the org tells you which currency

`Employee.Salary`, `HourlyRateKobo`, `PayrollItem.Amount`, `EWAAdvance.AmountKobo`, etc. are all still bare `money.Kobo` — a minor-unit integer with no currency of its own. `Organization.Currency` (migration 000029) is what gives that integer meaning: every one of those fields belongs to some org, and that org's currency is what it's denominated in. It's set once at creation (default NGN) and a DB trigger rejects any later UPDATE — changing it out from under an org's existing ledger history would corrupt the ledger's per-account currency invariant (next section), not convert anything.

- `models.OrgCurrencyTx(tx, orgID)` resolves it — call this, don't hardcode `money.NGN`, anywhere you're about to post a ledger entry, pick a default policy amount, or select a payment provider for an org.
- `money.KoboIn(currency, k)` is `NGNFromKobo`'s general form — the bridge from a Kobo field to the ledger's currency-tagged `Money`, once you know which currency.
- `provider.Registry.Select(currency)` fails loudly (`ErrNoProviderForCurrency`) if nothing registered can settle it. `PayrollService.CreatePayroll` calls this *before* creating anything, so an org configured with a currency no provider supports gets a clear synchronous error instead of a payroll that queues successfully and then fails deep inside the async worker.
- Both registered providers (Monnify, Paystack) are NGN-only today, and `payroll_worker.go`'s bulk-transfer path calls Monnify directly rather than through `provider.Registry` (bulk has no equivalent there — see `provider.TransferRequest`'s doc comment on why it's single-recipient only). Its `CurrencyCode: "NGN"` is deliberate, not a bug: `CreatePayroll`'s guard above is what keeps every payroll reaching that worker actually NGN. Don't remove the guard without also giving that worker a real multi-currency disbursement path.
- BVN is Nigeria's CBN-specific KYC check (migration 000002) — `CreateEmployee` only requires it for an NGN org. Don't make it unconditionally required again; that blocks onboarding for every other currency's employees.
- `DefaultEWAPolicy(orgID, currency)` picks illustrative, currency-appropriate starting amounts (`defaultEWAPolicyMajorUnits` in `internal/models/ewa.go`) — not derived from live FX. Ops still tunes real numbers per org via `UpdatePolicy`, same as for an NGN org.

## Direct-to-consumer: a D2C worker is a single-employee organization

`Organization.IsD2C` (migration 000030) flags a worker onboarded with no employer running payroll for them. They're modelled as their own organization with exactly one Employee record (themself) — not a new parallel identity system. This was a deliberate reuse, not a shortcut: `User`, `Employee`, every RLS policy, `LedgerAccount`'s identity index, and the JWT claims all structurally require an `organization_id` already (checked before building this — there's no precedent anywhere for data scoped to a person but no org, except `audit_events`' `organization_id IS NULL` system-event carve-out). Building a second, non-RLS'd tenant model for one product line would have been the premature abstraction; treating the individual as the tenant boundary they already are is not.

- D2C has no payroll relationship to draw eligibility or recourse from. Eligibility comes from `services.PredictNextPayday` (observed bank deposit history) via `EWAService.d2cIncomeBasisTx`, not `AccruedToDate`/`accruedHourlyToDateTx` — `eligibilityTx` branches on `Organization.IsD2C` before its salaried/hourly split, skipping the on-file-salary check (`Employee.Salary` is legitimately zero for a D2C worker) and computing basis + accrued straight-line across the predicted pay cycle instead (`d2cAccruedToDate`, `AccruedToDate`'s calendar-day analogue). No linked account, no `EWAService.D2CProvider` configured, or `PredictNextPayday`'s own `ErrInsufficientPaydayHistory` all block the same way (`DeclineNoIncomeHistory`) — never a fallback guess. `D2CProvider` is an optional field on `EWAService`, not a constructor parameter, specifically so `NewEWAService()`'s many existing call sites stay untouched; it's `nil` (and D2C orgs block cleanly) unless `routes.go` wires it, which it only does under `MOCK_MODE`. Settlement, unlike eligibility, was wired earlier: `services.InitiateD2CCollection` + `models.ConfirmD2CCollectionSuccess`/`Failure` (see below), not `SettleAdvancesForPayrollItem`, which stays payroll-triggered and never fires for a D2C org (nothing ever calls `CreatePayroll` for one).
- `banklink.Provider` is deliberately read-only (link + fetch transaction history). `banklink.DebitProvider` (embeds `Provider`) is the separate capability that can actually move money — `AuthorizeDebitMandate`/`InitiateDebit`/`GetDebitStatus`. Keep that split: a caller needing only payday-prediction reads should never be handed something that can also debit, even where a concrete adapter (none real exists yet — `Mock` only) implements both. `d2c_bank_links.debit_mandate_ref` is a second, separate, later consent from linking the account — don't collapse the two into one authorization step.
- `PredictNextPayday` returns `ErrInsufficientPaydayHistory` rather than a low-confidence guess whenever the evidence doesn't clear its bar. Never relax that into a fallback estimate — a direct debit scheduled against a wrong guess pulls money from a worker's account on a day nothing is actually there.
- Collection failure is an *ordinary* outcome, not a system error — `ConfirmD2CCollectionFailure` leaves the advance `Disbursed` for a later predicted-payday retry, up to `maxD2CCollectionAttempts` (4, in `internal/models/d2c_ledger.go`), only then calling `WriteOffAdvance`. Don't treat a failed debit as cause to write off immediately — a worker without funds on one predicted day is not the same as a worker who will never pay.
- `services.SweepD2CCollections` re-runs `PredictNextPayday` fresh every sweep rather than caching a prediction — a worker's actual pay cadence drifting must be reflected on the very next run, not stale until someone notices.
- **Regulatory posture is unresolved for D2C** — see `docs/EWA_ROADMAP.md` §7's new note. The org-funded model's entire "not lending" argument rests on no-recourse-to-the-worker; direct debit on a predicted payday is recourse. `IsD2C` makes the product line explicit in code; it is not a legal opinion. Don't point a real `DebitProvider` at production without counsel — `cmd/api/main.go`'s `collect-d2c-debits` mode already refuses to run outside `MOCK_MODE` for exactly this reason; don't remove that guard when a real provider is wired without confirming the regulatory question first.
- `POST /api/v1/d2c/signup` (`D2CHandler.Signup`) is the only API that creates an `Organization` — every other org is still ops-created. It's public (no identity exists yet to gate it behind), creates org + employee + user + a `d2c_direct_debit_disclosure` consent in one transaction, and returns a worker JWT directly rather than a separate OTP login step. It does **not** record bank-link or debit-mandate consent — those belong at the linking step (still unbuilt as an API; see below), not at signup, since no account has been chosen yet.
- Migration 000002's `CREATE TABLE IF NOT EXISTS organizations (...)` was a silent no-op against the table 000001 already created — `password_hash`/`role`/`is_active` never actually existed on any freshly-migrated database until migration 000032 added them via `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`. If you ever see "column ... of relation organizations does not exist" for one of those three, this is why — the fix is already shipped, don't re-diagnose it as new.
- `POST /api/v1/worker/d2c/bank-link/{initiate,complete,authorize-debit}` (`D2CBankLinkHandler`) wrap `banklink.Provider.InitiateLink`/`CompleteLink` and `DebitProvider.AuthorizeDebitMandate`. `routes.go` only registers this group when `MOCK_MODE=true` — deliberately, since exposing a `Mock`-backed linking flow to a real user ahead of a real aggregator would hand them a canned "success" for an account that was never actually linked. Don't remove that guard until `banklink.NewMock()` there is replaced with a real provider. `complete` records `d2c_bank_link_read` consent; `authorize-debit` records a separate `d2c_debit_mandate` consent and requires an existing `D2CBankLinkLinked` row — never collapse the two steps or their consents into one.

## Tenant isolation: RLS, not WHERE clauses

Every tenant-scoped DB write or read must run inside `models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error { ... })`. The helper opens a transaction and sets `app.org_id` via `set_config(..., true)`; Postgres RLS policies (migrations 000008–000011) then filter rows server-side. A forgotten WHERE clause returns zero rows, not another tenant's data.

- Don't write raw `models.DB.Where("organization_id = ?", orgID)...` queries. Use `WithOrgScope` + `repo.WithTx(tx)`.
- The only legitimate unscoped read is the webhook's `lookup_payroll_item_for_webhook(ref)` call (SECURITY DEFINER function, migration 000011). Reason: Monnify callbacks don't carry orgID, so we need a single-UUID read to learn it. All subsequent writes are scoped via the loaded `item.OrganizationID`.
- The `audit_events` policy allows `organization_id IS NULL` for system-level events. Pass `""` to `AppendAuditTx` for those.

## The ledger is the source of truth for value

Every movement of value posts balanced entries through `models.PostTransaction`.
Balances are **derived** by summing entries (`models.AccountBalance`) — never
stored. A stored balance is a cache that drifts; a derived one cannot.

- `debits = credits` is enforced by a DEFERRABLE constraint trigger that fires at
  COMMIT (migration 000013), not by application code. `ValidatePosting` runs the
  same check earlier only to produce a better error message.
- `ledger_entries` is append-only — a trigger rejects UPDATE and DELETE.
  Corrections are new reversing entries.
- Entry amounts are strictly positive; `direction` carries the sign.
- Every posting needs an idempotency key, uniquely indexed per org, so a retried
  disbursement is a no-op instead of a double-post.

Don't add a mutable balance column. If you need a balance, sum the entries.

## EWA: advances are claims on wages already earned

`internal/services/ewa_service.go` recomputes eligibility **inside the same
transaction that writes the advance** — a client never supplies its own cap, and
a concurrent request cannot race past one checked a moment earlier.

- Accrual is straight-line over *working* days (`AccruedToDate`) for salaried
  staff. Hourly/gig staff (`Employee.WageType == "hourly"`) accrue instead from
  real approved `TimeEntry` rows (`accruedHourlyToDateTx`, migration 000018) —
  never an assumed schedule. An unapproved entry counts toward nothing:
  accrual, payroll, and dependency scoring all read only `status = 'approved'`.
- `accruedHourlyToDateTx` deliberately stays flat-rate (`HourlyRateKobo × minutes`),
  even though actual payroll no longer is — `services.ComputeHourlyGross`
  (migration 000027) applies `PayrollPolicy` shift differentials and weekly
  overtime there. Eligibility only needs a conservative floor on wages already
  earned; the exact figure settles at payroll time regardless. Don't wire
  ComputeHourlyGross into eligibility without deciding whether an EWA draw
  should be able to anticipate an overtime premium that hasn't happened yet.
- `accruedHourlyToDateTx` also stays period-scoped (`ApprovedEntriesForPeriod`)
  even though payroll no longer is: `ComputeHourlyGross` sweeps unpaid
  approved entries from ANY earlier period too (`UnpaidApprovedEntriesThrough`,
  migration 000028 — `time_entries.paid_payroll_item_id`), so a late-approved
  entry from last period still gets paid by whichever run catches it. That
  sweep is a payroll-gross concern, not an eligibility one — don't wire it in
  without deciding whether accrual should count wages from a period whose
  payroll has already run.
- Advances settle by netting out of the next payroll item, inside the payroll
  creation transaction (`SettleAdvancesForPayrollItem`). An advance that does not
  settle is money the business never recovers.
- **Only `disbursed` advances are deducted from wages.** An advance that was
  approved but never actually paid out is *cancelled* with a reversing ledger
  entry and withholds nothing. Deducting pay for money the worker never received
  is wage theft, not an accounting detail. `approved → settled` is deliberately
  absent from the FSM so this cannot be done by accident.
- Guardrails live in `ewa_dependency.go`. `ScoreDependency` is a pure function —
  keep it that way, it is the only reason the thresholds are testable.
- **`TierCap` must never return zero.** Cutting off a worker in need moves them to
  a payday lender at multiples of our cost; it does not remove the need. Every
  tier retains `EmergencyFloorKobo`, and a test asserts it. If you find yourself
  changing that test, it is the wrong fix.
- Employer funding coverage (`ewa_policies.require_funding_coverage`, migration
  000019) is opt-in and off by default. `models.FundingExposure` reads
  `cash_settlement`'s own balance as "cumulative advances minus cumulative
  employer deposits" — deliberately no new account type and no change to
  `postAdvanceLedger`'s existing entries; see the migration comment before
  touching either of those two accounts' meaning.
- No fee, no recourse beyond payroll deduction, no collections, no credit
  reporting. That is the regulatory position, not only an ethical preference —
  see `docs/EWA_ROADMAP.md` §7. `fee_kobo` exists and defaults to zero; enabling
  it needs legal review.
- Dependency data goes to employers in aggregate only. Per-worker exposure is a
  retaliation vector.

## FSM transitions and atomicity

`models.TransitionStatus(tx, row, current, next)` is a CAS UPDATE. If `RowsAffected == 0` it returns `models.ErrStaleStatus` — treat that as idempotent success in concurrent paths (webhook), or as a duplicate-task abort (worker).

Counter init and FSM transition for a payroll happen as one atomic UPDATE in the worker (`UPDATE payrolls SET status=processing, pending_count=N WHERE id=? AND status IN (pending, failed)`). Never split that into separate statements — webhooks racing during the Monnify call depend on `pending_count` already being correct.

`pending_count` counts only items that can still decrement it. The worker submits
and counts **unsettled items only** — the `failed → processing` retry edge means a
batch re-enters the worker still carrying items Monnify already settled. Counting
or re-sending those pays a worker twice and leaves the counter unable to reach
zero. Items whose employee is soft-deleted are failed rather than skipped, for the
same reason: a skipped item holds the counter above zero forever.

The webhook amount is decimal **Naira** on the wire. Parse it with
`money.FromNairaString`, never straight into `money.Kobo` — `Kobo.UnmarshalJSON`
reads a bare number as minor units, which is a 100× error. The reported amount is
checked against the stored item amount before the item is settled.

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
