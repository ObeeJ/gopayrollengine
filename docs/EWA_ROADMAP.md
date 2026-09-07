# Earned Wage Access — Research, Product Plan, and Roadmap

Owner: platform / payments
Status: Phase 1 implemented, Phases 2–5 planned
Last updated: 2026-08

---

## 0. How to read this, and what it is not

**On the evidence.** The figures in §1 come from regulator and industry research.
They were gathered under a network egress policy that blocked direct fetches of
`consumerfinance.gov`, `federalregister.gov`, `60decibels.com`, and several law-firm
analyses. The numbers below therefore come from **search-result summaries of those
primary sources, not from the primary documents themselves.** Every figure marked
⚠️ must be re-verified against the source PDF before it is used in a board deck,
an investor conversation, or any external claim.

**On community sentiment.** The brief asked for Reddit, YouTube, and X opinion
mining. That could not be done properly from this environment: the available search
tooling would not return `site:reddit.com` results, and general queries returned
overwhelmingly vendor marketing content (DailyPay, Earnin, Tapcheck blogs) rather
than user discussion. What appears in §2 under "worker voice" is therefore
**directional and secondary**, inferred from research summaries rather than read
first-hand. It is good enough to design guardrails against. It is **not** good
enough to claim we know what users think. §6 Phase 0 makes primary research a
funded work item rather than pretending it is done.

Being straight about this matters more than looking thorough: the entire guardrail
design rests on a claim about user behaviour, and we should know exactly how well
evidenced that claim is.

---

## 1. What the data actually says

### Market size and trajectory

| Metric | Value | Source |
|:---|:---|:---|
| US workers using employer-partnered EWA (2022) | >7 million ⚠️ | CFPB paycheck advance data spotlight |
| Value accessed (2022) | ~$22 billion ⚠️ | CFPB |
| Transaction growth 2021 → 2022 | >90% ⚠️ | CFPB |
| Nigeria: Earnipay funding / reach | $4M seed, 150+ companies | Techpoint Africa, NIPC |
| Nigeria: typical access limit | up to 50% of earned wages | Earnipay product terms |
| Nigeria: typical fee | flat ₦250–₦500 per draw | Earnipay product terms |

This is a fast-growing category with real demand. That is not in dispute.

### The number that should shape the product

| Metric | Value | Source |
|:---|:---|:---|
| Average advances per user per year | ~27 ⚠️ | CFPB |
| Typical user | ~36/yr; heavy users >100/yr ⚠️ | Industry research |
| Users drawing at least monthly | ~50% in 2022, up from 41% in 2021 ⚠️ | CFPB |
| Usage trajectory in first year | roughly doubles: ~2/mo → ~4/mo ⚠️ | Industry research |
| Users re-drawing same or next day after repayment | ~75% ⚠️ | Industry research |
| Users drawing for **routine bills**, not emergencies | 50–78% ⚠️ | Multiple surveys |
| Workers paying ≥1 fee where employer doesn't cover | >90% ⚠️ | CFPB |
| Share of fee revenue from expedited transfer | 92.5% ⚠️ | CFPB |
| Typical expedited fee | $3.18 avg, $1–$5.99 range ⚠️ | CFPB |
| Illustrative APR, employer-partnered advance | ~109.5% ⚠️ | CFPB |

**Read those four rows together and the product thesis inverts.**

EWA is sold as emergency liquidity — a bridge over an unexpected shock. The data
says that for most users it is not that. It is a *permanent forward shift of the
pay cycle*: usage roughly doubles in the first year, three-quarters of users
re-draw immediately after being repaid, and a majority use it for routine bills.
Once someone has drawn 40% of their wages early, every subsequent period starts
40% short — and the only way to close that gap is to draw again. The behaviour is
self-reinforcing by construction.

That is the pain point worth solving, and nobody in the category is solving it.
Competitors optimise for **draw volume**, because in a per-transaction fee model
draw volume *is* revenue. That aligns the provider's income with the deepening of
the user's dependency. It is the payday-lending incentive with better branding.

### The counter-argument, taken seriously

There is real evidence on the other side, and it should not be dismissed:

- Employer-partnered EWA is materially cheaper than the alternatives it displaces
  — payday loans, overdraft fees, late fees. Research on retention (HBS working
  paper on EWA and employee retention) finds meaningful employer-side benefit.
- Some provider data shows most users draw modest amounts for necessities, not
  discretionary spending.
- A worker facing a genuine shortfall who is **denied** access does not stop
  needing the money. They go somewhere worse.

**This is why the guardrail design is graduated rather than prohibitive.** A hard
block is not a safety feature; it is a redirection to a 300%+ APR lender. The
implemented design tightens caps and adds friction as dependency deepens but
always preserves an emergency floor. See `internal/services/ewa_dependency.go`.

---

## 2. Pain points by stakeholder

### Workers

| Pain | Evidence strength | Addressed by |
|:---|:---|:---|
| Pay cycle doesn't match expense timing | Strong | Core EWA (Phase 1) |
| Fees are opaque; effective cost invisible | Strong ⚠️ | Zero-fee model; explicit cost display |
| "Expedited" fee is the real charge — free tier is slow | Strong ⚠️ | No tiering; one speed, no fee |
| Gradual dependency nobody warns you about | Strong ⚠️ | Dependency scoring + tiered response |
| Payday arrives already spent | Strong ⚠️ | Cap below 100% of accrual; headroom by design |
| Can't see why a limit is what it is | Directional | Full eligibility breakdown returned by API |

### Employers

| Pain | Evidence strength | Addressed by |
|:---|:---|:---|
| Payroll reconciliation breaks with bolt-on EWA | Strong | Settlement inside the payroll transaction |
| Unclear whether it's a wage deduction (legal risk) | Strong | Ledger evidence + audit trail |
| Admin burden each pay period | Strong | Automatic netting; no manual step |
| Vendor failure reflects on the employer | Strong | Native to payroll, not a third party |
| No visibility into workforce financial stress | Gap in market | Aggregate dependency tiers (never per-worker without consent) |

### The unserved gap

Employers currently get **no signal at all** about workforce financial strain
until someone resigns. Aggregate, anonymised dependency-tier reporting — "18% of
your workforce moved from healthy to strained this quarter" — is a genuinely new
product surface, and it is a by-product of the guardrails we already need.

Hard constraint: this must be **aggregate and anonymised**, always. Per-worker
dependency data exposed to an employer is a discrimination and retaliation vector.
It is a firing signal wearing a wellness lanyard. Not negotiable.

---

## 3. Product principles

1. **You can only access wages you have already earned.** Recomputed server-side
   from working-day accrual, never supplied by the client.
2. **No fee on the core product.** No per-draw fee, no expedited tier, no tips.
   Removes the incentive to maximise draw frequency and keeps us clear of the
   "this is credit" analysis.
3. **No recourse beyond payroll deduction.** No collections, no credit reporting,
   no debiting a personal bank account. Encoded as an absent capability.
4. **Guardrails narrow access; they never close it.** Every tier keeps an
   emergency floor.
5. **Every limit is explainable.** The API returns the full basis for the number.
6. **Employers see aggregates. Never individuals.**

Principles 2 and 3 are also the regulatory strategy — see §7.

---

## 4. What is built (Phase 1, in this repository)

| Capability | Location |
|:---|:---|
| Double-entry ledger, balances derived from entries | `internal/db/migrations/000013_*.sql`, `internal/models/ledger.go` |
| Debits = credits enforced at COMMIT by deferred trigger | `000013` — verified against Postgres 16 |
| Entries append-only (UPDATE/DELETE rejected by trigger) | `000013` — verified |
| Working-day accrual engine | `AccruedToDate` in `internal/services/ewa_service.go` |
| Eligibility: accrual cap, absolute cap, outstanding netting | `eligibilityTx` |
| Velocity limits + cooling-off | `eligibilityTx`, `TierCoolingOff` |
| Dependency scoring (4 signals, 0–100, 4 tiers) | `internal/services/ewa_dependency.go` |
| Graduated tier response with emergency floor | `TierCap` |
| Settlement netted out of payroll, same transaction | `SettleAdvancesForPayrollItem`, `payroll_service.go` |
| Undisbursed advances cancelled, never deducted from wages | `SettleAdvancesForPayrollItem` |
| Advance FSM with terminal states | `internal/models/ewa.go` |
| Per-org configurable policy | `ewa_policies` table |
| RLS tenant isolation on every new table | `000013`, `000014` |

**Dependency signals** (rolling 90-day window):

| Signal | Max | Detects |
|:---|---:|:---|
| Frequency | 30 | Draws per 30 days |
| Utilization | 30 | Share of earnings drawn early |
| Escalation | 20 | Month-over-month growth — the "doubles in year one" trajectory |
| Immediacy | 20 | Draws within 72h of payday — the "75% re-draw" signal |

**Tier response:**

| Tier | Score | Cap | Cooling-off |
|:---|:---|:---|:---|
| healthy | 0–39 | full | base (24h) |
| elevated | 40–59 | full + nudge | base |
| strained | 60–79 | 50% | doubled |
| dependent | 80–100 | emergency floor | doubled |

### Defects fixed alongside

Found during the audit that preceded this work:

- **Double disbursement on retry.** The `failed → processing` edge re-submitted
  already-settled items — a transient gateway error could pay a worker twice.
- **Permanently stranded batches.** `pending_count` was reset to the full item
  count on retry, but settled items no longer emit webhooks, so it could never
  reach zero.
- **100× unit error.** The Monnify webhook decoded decimal *Naira* directly into
  `money.Kobo`, which reads a bare number as minor units.
- **No amount verification.** The webhook never checked that the amount the
  gateway reported matched the amount requested.
- **`RequestAdvance` was entirely non-functional** — it never set `UserID`, which
  is `NOT NULL` with a foreign key, so every request failed.
- **Integer overflow** in `FromNairaString` (reachable from request bodies),
  `Add` (MinInt64 pair), and `Sub` (implemented as `Add(-other)`).

---

## 5. Metrics

### Product health — the ones that matter

The temptation is to report draw volume and call it engagement. For this product
that is exactly backwards: rising draw volume per user is the **failure** signal.

| Metric | Direction | Why |
|:---|:---|:---|
| **Share of active users in healthy tier** | ↑ | The headline metric |
| Net tier migration (healthy→strained per quarter) | ↓ | Leading indicator of harm |
| Median draws per user per period | ↓ or flat | Rising = habituation |
| Median utilization (drawn ÷ earned) | flat, <30% | Payday should not arrive empty |
| % of draws within 72h of payday | ↓ | The dependency tell |
| % of users who *stop* needing EWA | ↑ | Genuine success |
| Draw volume | — | Reported, never optimised |

Instrumented in `internal/observability/metrics.go`:
`payroll_ewa_dependency_tier_employees`, `payroll_ewa_utilization_ratio`,
`payroll_ewa_decline_reasons_total`.

### Unit economics — illustrative, not measured

**These are assumptions with arithmetic applied, not findings.** They exist to
frame the funding question, and every input needs replacing with real figures
before anyone relies on them.

Zero-fee to the worker means the employer or the platform funds it. Per 1,000
enrolled workers, assuming ~30% monthly activation and a ₦25,000 median draw:

- Advanced per month: ~300 × ₦25,000 = **₦7.5M** outstanding float
- Float duration: ~15 days average → ~₦3.75M average committed capital
- At 20% annualised cost of capital: **~₦62,500/month** funding cost
- Plus transfer fees: 300 × ~₦50 = **₦15,000/month**
- **Total ≈ ₦77,500/month per 1,000 workers ≈ ₦78/worker/month**

That is the number to price against as an employer benefit (SaaS per-seat) rather
than as a per-draw worker fee. It is plausibly viable. It is **not validated** —
activation rate, median draw, and float duration are all guesses, and the model is
highly sensitive to all three.

---

## 6. Roadmap

### Phase 0 — Validate the premise (do this before Phase 2)

The guardrail design rests on behavioural claims taken from secondary sources.
Before building further on it:

- Primary user research: 30+ interviews across Nigerian salaried workers, split
  between EWA users and non-users
- Systematic community analysis — actual Reddit/X/YouTube corpus, properly
  sampled, not search summaries
- Employer interviews: 10+ HR/payroll leads on reconciliation and liability
- Re-verify every ⚠️ figure against its primary source
- Legal review of §7 with a Nigerian financial services counsel

**Exit criteria:** dependency thresholds either confirmed or recalibrated against
real data. The current thresholds are reasoned, not measured, and should be
treated as provisional.

### Phase 1 — Core engine ✅ complete

Ledger, accrual, eligibility, guardrails, settlement, tenant isolation.

### Phase 2 — Make it real

- ~~Disbursement execution: EWA draws currently post to the ledger but are not
  yet wired to an actual Monnify transfer.~~ — done: provider abstraction
  (Monnify + Paystack) with idempotent disbursement and webhook confirmation.
- ~~Funding pool balance checks before approval~~ — done: `organization_funding_accounts`
  (migration 000019) gives each org a dedicated deposit account; a deposit
  credits `employer_funding` and offsets `cash_settlement`, and
  `ewa_policies.require_funding_coverage` (opt-in, off by default) blocks a
  draw that would push the org's exposure past what it has funded. Still
  open: no reconciliation job cross-checking the ledger against the
  provider's own statement of the account (Phase 3's reconciliation item
  covers payroll disbursement the same way and should absorb this too).
- Worker-facing nudge content for the elevated tier
- Accrual snapshot cron (`ewa_accrual_snapshots` is defined but not yet populated)
- Admin API for policy configuration

### Phase 3 — Close the loops

- Reconciliation job: ledger balances vs. Monnify statements, alerting on drift
- Write-off path for terminated employees with outstanding advances
- Partial settlement when net pay is insufficient
- Multi-period advances (draw in August, settle across September/October)
- Aggregate employer dashboard (anonymised, k-anonymity threshold ≥ 10)

### Phase 4 — Beyond the shift

The dependency score identifies people the product cannot help by giving them
more of the same. That is where the actual differentiation is:

- Automated savings: round-up or a fixed share diverted at payroll
- Bill-timing tools — much repeat usage is a timing mismatch, not a shortfall
- Referral to financial counselling at the strained tier
- Employer-funded hardship grants as a genuine alternative to a fourth advance

### Phase 5 — Scale

- ~~Hourly/shift accrual from real timesheet data~~ — done: `TimeEntry`
  (migration 000018) accrues hourly/gig staff from approved timesheet entries,
  never an assumed schedule. Still open: no shift-differential or overtime
  pay rules, and payroll only pays whole approved entries closed out before
  the run — a late approval after payroll has already run for that period
  is not swept up retroactively.
- Multi-currency, multi-country
- Direct-to-consumer (much harder: no payroll deduction, so no recourse-free model)

---

## 7. Regulatory posture

**Nigeria (primary market).** No EWA-specific CBN framework is currently in
evidence. The category operates adjacent to existing lending and payments rules.
The defensible position is to be structurally *not lending*:

- Advance is limited to wages already earned — no credit extended
- No interest, no fee
- Recovery only by payroll deduction — no recourse to the worker
- No credit reporting, no collections
- Employer is the counterparty for funding

**United States (relevant as precedent).** The CFPB proposed an interpretive rule
in 2024 treating paycheck advance products as credit under Regulation Z. That
position **reversed**: by late 2025 / early 2026 the posture moved to
non-application of Reg Z for qualifying EWA products. ⚠️ The specific qualifying
conditions were not directly readable from this environment and must be confirmed
against the Federal Register text before being relied on.

The direction of travel across both proposed and final positions is consistent:
**products that charge no fee and have no recourse sit outside the credit
perimeter; products with mandatory fees or recovery rights get pulled inside it.**

Principles 2 and 3 in §3 are therefore not only an ethical stance — they are the
cheapest available regulatory position. A fee-charging variant is deliberately
*possible* in the schema (`fee_kobo`, `AccountFeeIncome`) but defaults to zero,
because turning fees on should be a decision with legal review attached, not a
config change nobody noticed.

**NDPR / data protection.** Dependency scoring is profiling of a financial
nature. Requires: explicit consent (the `consent_records` table exists),
worker access to their own score and its inputs (the API returns them),
and no automated decision without explanation (decline reasons are returned
and recorded).

---

## 8. Risks

| Risk | Severity | Mitigation |
|:---|:---|:---|
| Guardrail thresholds are wrong; harms real users | High | Phase 0 validation; thresholds are per-org configurable |
| Dependency score used as an employment signal | High | Aggregate-only reporting, k≥10; never per-worker to employer |
| Advance exceeds accrual through an accrual bug | High | Cap ≤ accrual asserted in tests; negative net pay hard-stops the payroll run |
| Float funding dries up mid-period | High | Phase 2 pool balance checks before approval |
| Ledger drifts from gateway reality | Medium | Phase 3 automated reconciliation |
| Zero-fee model proves unviable | Medium | Per-seat employer pricing; fee path exists but off by default |
| Regulatory reclassification as credit | Medium | Structural non-lending design; no fee, no recourse |
| Hourly workers get bad accrual | Low | Real timesheet-based accrual (000018); residual risk is admins rubber-stamping unverified entries, not the engine |

**The risk being accepted deliberately:** graduated guardrails will let some users
reach a level of dependency a hard cap would have prevented. That is a real cost.
It is accepted because the alternative — cutting people off — pushes them to
lenders charging multiples of our cost, and we would never see that harm in our
metrics. Choosing the harm you can measure over the harm you cannot is a bias, not
a safety strategy.

---

## Sources

- [CFPB — Data Spotlight: Developments in the Paycheck Advance Market](https://www.consumerfinance.gov/data-research/research-reports/data-spotlight-developments-in-the-paycheck-advance-market/)
- [Federal Register — Truth in Lending (Reg Z); Non-application to Earned Wage Access Products](https://www.federalregister.gov/documents/2025/12/23/2025-23735/truth-in-lending-regulation-z-non-application-to-earned-wage-access-products)
- [CFPB — Proposed interpretive rule on paycheck advance costs and fees](https://www.consumerfinance.gov/about-us/newsroom/cfpb-proposes-interpretive-rule-to-ensure-workers-know-the-costs-and-fees-of-paycheck-advance-products/)
- [Goodwin — CFPB Brings Clarity to Earned Wage Access Products](https://www.goodwinlaw.com/en/insights/blogs/2026/01/cfpb-brings-clarity-to-earned-wage-access-products)
- [HBS — Fintech to the (Worker) Rescue: Earned Wage Access and Employee Retention](https://www.hbs.edu/ris/Publication%20Files/FinTech%20to%20the%20Worker%20Rescue%20-%20Earned%20Wage%20Access%20and%20Employee%20Retention_2d9994e9-705d-499c-8d27-6ffb98d5ee14.pdf)
- [UW Evans School — Earned Wage Access Financial Services (June 2025)](https://evans.uw.edu/wp-content/uploads/2025/08/EWA-Report-1.pdf)
- [60 Decibels — How Earned Wage Access is Reshaping Financial Habits](https://60decibels.com/insights/how-earned-wage-access-is-reshaping-financial-habits/)
- [Techpoint Africa — Money now, not later: earned wage access in Nigeria](https://techpoint.africa/insight/earned-wage-access-nigeria/)
- [NIPC — Earnipay secures $4m seed round](https://www.nipc.gov.ng/2022/02/18/nigerian-fintech-startup-earnipay-secures-4m-seed-round-to-scale-earned-wage-access-solution/)
- [Afridigest — Earned wage access, explained](https://afridigest.com/primer-earned-wage-access/)
- [Boston University Law Review — Hawkins on earned wage access regulation](https://www.bu.edu/bulawreview/files/2021/04/HAWKINS.pdf)
- [Your Money Line — EWA: Treating the Symptom, Not the Problem](https://www.yourmoneyline.com/blog/earned-wage-access-treating-the-symptom-not-the-problem)
- [Excelforce — EWA benefits, risks, compliance, and payroll integration](https://www.excelforce.com/insights/earned-wage-access-guide-employers)
