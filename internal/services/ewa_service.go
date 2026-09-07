package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/observability"
	"go-payroll-engine/internal/workers"
	"go-payroll-engine/pkg/money"

	"github.com/hibiken/asynq"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PeriodLayout is the payroll period format: "2026-08".
const PeriodLayout = "2006-01"

// Decline reasons. Stable strings — they are metric labels and worker-facing keys.
const (
	DeclineEWADisabled     = "ewa_disabled"
	DeclineBelowMinimum    = "below_minimum"
	DeclineExceedsEarned   = "exceeds_earned"
	DeclineCoolingOff      = "cooling_off"
	DeclineDrawLimit       = "draw_limit_reached"
	DeclineNoSalaryOnFile  = "no_salary_on_file"
	DeclineInactiveAccount = "inactive_account"
	// DeclineFundingPoolExhausted fires only when the org has opted into
	// require_funding_coverage and has drawn more than it has deposited —
	// see FundingExposure. Distinct from DeclineExceedsEarned so a worker
	// isn't told they haven't earned enough when the real cause is their
	// employer's funding, not their own accrual.
	DeclineFundingPoolExhausted = "funding_pool_exhausted"
)

// ErrAdvanceDeclined is returned when a request fails a guardrail. The advance
// row is still written with status=declined so the decision is auditable.
var ErrAdvanceDeclined = errors.New("advance declined")

// EWAService owns earned wage access: what a worker has earned, what they may
// draw, and what happens when they draw it.
type EWAService struct{}

// NewEWAService constructs the service.
func NewEWAService() *EWAService { return &EWAService{} }

// AccruedToDate returns wages earned so far in the period, for salaried staff.
//
// Accrual is straight-line across *working* days (Mon–Fri), not calendar days:
// a worker three days into a month has not earned 10% of a monthly salary, and
// paying as if they had is how an EWA product ends up advancing unearned wages.
// This assumes a fixed monthly salary; hourly and gig workers use
// accruedHourlyToDateTx instead, which sums real approved timesheet entries
// rather than approximating a schedule — see migration 000018.
func AccruedToDate(salary money.Kobo, period string, asOf time.Time) (money.Kobo, error) {
	start, err := time.ParseInLocation(PeriodLayout, period, asOf.Location())
	if err != nil {
		return 0, fmt.Errorf("invalid period %q: %w", period, err)
	}
	if !salary.IsPositive() {
		return 0, nil
	}

	total := workingDaysBetween(start, start.AddDate(0, 1, 0))
	if total == 0 {
		return 0, nil
	}

	// Before the period starts nothing is earned; after it ends the whole salary is.
	if asOf.Before(start) {
		return 0, nil
	}
	end := start.AddDate(0, 1, 0)
	if !asOf.Before(end) {
		return salary, nil
	}

	// Count the day in progress as earned only once it is over — accruing a day
	// at 00:01 would advance wages for work not yet done.
	elapsed := workingDaysBetween(start, truncateToDay(asOf))
	if elapsed <= 0 {
		return 0, nil
	}
	return salary.Percent(int64(elapsed), int64(total))
}

// workingDaysBetween counts Mon–Fri days in [from, to).
func workingDaysBetween(from, to time.Time) int {
	count := 0
	for d := truncateToDay(from); d.Before(to); d = d.AddDate(0, 0, 1) {
		if wd := d.Weekday(); wd != time.Saturday && wd != time.Sunday {
			count++
		}
	}
	return count
}

func truncateToDay(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// accruedHourlyToDateTx sums an hourly/gig employee's approved timesheet
// minutes for `period` up to asOf and converts them to wages at their hourly
// rate. Unapproved entries never count — see migration 000018's comment on
// why an unverified timesheet claim must not fund an advance.
func (s *EWAService) accruedHourlyToDateTx(tx *gorm.DB, orgID, employeeID string, hourlyRate money.Kobo, period string, asOf time.Time) (money.Kobo, error) {
	minutes, err := models.SumApprovedMinutes(tx, orgID, employeeID, period, asOf)
	if err != nil {
		return 0, err
	}
	return hourlyRate.Percent(minutes, 60)
}

// hourlyMonthlyEarningsBasisTx estimates a stable "typical month" figure for
// an hourly/gig worker from their actual approved earnings over the trailing
// 90 days, rather than assuming a standard full-time schedule. Used only as
// the dependency-scoring denominator and the number shown as "monthly
// salary" — never as a prediction of this period's final pay, which is
// accrued-to-date's job (see the periodEarningsBasis comment in eligibilityTx).
func (s *EWAService) hourlyMonthlyEarningsBasisTx(tx *gorm.DB, orgID, employeeID string, hourlyRate money.Kobo, asOf time.Time) (money.Kobo, error) {
	// work_date is a DATE column; bounds are rounded to whole days so the
	// comparison is exact regardless of how the driver types the parameter —
	// see the comment on models.SumApprovedMinutes for why a sub-day instant
	// bound against a DATE column is ambiguous.
	asOfDate := truncateToDay(asOf)
	windowStart := asOfDate.AddDate(0, 0, -90)
	upperExclusive := asOfDate.AddDate(0, 0, 1)
	var totalMinutes int64
	err := tx.Model(&models.TimeEntry{}).
		Where("organization_id = ? AND employee_id = ? AND status = ?", orgID, employeeID, models.TimeEntryApproved).
		Where("work_date >= ? AND work_date < ?", windowStart, upperExclusive).
		Select("COALESCE(SUM(minutes_worked), 0)").
		Scan(&totalMinutes).Error
	if err != nil {
		return 0, err
	}
	earned, err := hourlyRate.Percent(totalMinutes, 60)
	if err != nil {
		return 0, err
	}
	// 90 days ≈ 3 months.
	return earned.Percent(1, 3)
}

// Eligibility — the full picture behind an allow/deny decision. Returned to the
// worker so the number they see is explained rather than asserted.
type Eligibility struct {
	EmployeeID    string     `json:"employee_id"`
	Period        string     `json:"period"`
	MonthlySalary money.Kobo `json:"monthly_salary"`

	AccruedToDate money.Kobo `json:"accrued_to_date"`
	PolicyCap     money.Kobo `json:"policy_cap"`
	TierCap       money.Kobo `json:"tier_cap"`
	Outstanding   money.Kobo `json:"outstanding"`
	Available     money.Kobo `json:"available"`
	MinimumDraw   money.Kobo `json:"minimum_draw"`

	// ProjectedPayday is what will actually land on payday if nothing further is
	// drawn, and ProjectedPaydayIfMaxDrawn if the whole remaining allowance is
	// taken. UK user research is unambiguous that people grasp "money available
	// now" far more readily than "next payday reduced" — they experience the
	// smaller paycheck as a surprise. Both numbers are returned so the interface
	// can show the consequence next to the offer rather than a screen later.
	ProjectedPayday           money.Kobo `json:"projected_payday"`
	ProjectedPaydayIfMaxDrawn money.Kobo `json:"projected_payday_if_max_drawn"`

	// ProtectedPayday is the worker's own floor, if they set one.
	ProtectedPayday money.Kobo `json:"protected_payday"`

	DrawsThisPeriod int        `json:"draws_this_period"`
	MaxDraws        int        `json:"max_draws_per_period"`
	NextEligibleAt  *time.Time `json:"next_eligible_at,omitempty"`

	Dependency DependencyAssessment `json:"dependency"`
	// Blocked is set when a guardrail currently prevents any draw.
	Blocked       bool   `json:"blocked"`
	BlockedReason string `json:"blocked_reason,omitempty"`

	// FundingBound is set when the employer's funding pool — not the
	// worker's own accrued wages — is the tightest constraint on Available.
	// RequestAdvance reads this to attribute a decline correctly even when
	// Available is still above MinimumDraw (so Blocked is false) but below
	// the specific amount requested — see the decision switch there.
	FundingBound bool `json:"-"`
}

// GetEligibility computes what a worker may draw right now, and why.
func (s *EWAService) GetEligibility(ctx context.Context, orgID, employeeID string, asOf time.Time) (*Eligibility, error) {
	var result *Eligibility
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		e, err := s.eligibilityTx(tx, orgID, employeeID, asOf)
		result = e
		return err
	})
	return result, err
}

// eligibilityTx does the work inside an existing RLS-scoped transaction, so the
// request path can compute eligibility and write the advance atomically.
func (s *EWAService) eligibilityTx(tx *gorm.DB, orgID, employeeID string, asOf time.Time) (*Eligibility, error) {
	var emp models.Employee
	if err := tx.First(&emp, "id = ?", employeeID).Error; err != nil {
		return nil, err
	}

	policy, err := s.policyTx(tx, orgID)
	if err != nil {
		return nil, err
	}

	period := asOf.Format(PeriodLayout)
	el := &Eligibility{
		EmployeeID:  emp.ID,
		Period:      period,
		MinimumDraw: policy.MinDrawKobo,
		MaxDraws:    policy.MaxDrawsPerPeriod,
	}

	if !policy.Enabled {
		el.Blocked, el.BlockedReason = true, DeclineEWADisabled
		return el, nil
	}
	if !emp.IsActive {
		el.Blocked, el.BlockedReason = true, DeclineInactiveAccount
		return el, nil
	}
	if emp.IsHourly() {
		if !emp.HourlyRateKobo.IsPositive() {
			el.Blocked, el.BlockedReason = true, DeclineNoSalaryOnFile
			return el, nil
		}
	} else if !emp.Salary.IsPositive() {
		el.Blocked, el.BlockedReason = true, DeclineNoSalaryOnFile
		return el, nil
	}

	// MonthlySalary is a stable "typical monthly income" reference — the
	// dependency utilization denominator, and what the API shows. For salaried
	// staff that's just the salary. Hourly/gig workers have no fixed figure, so
	// it is the actual trailing average rather than an assumed full-time month:
	// assuming one would overstate a part-time worker's income, and the
	// roadmap is explicit that hourly accrual "must not be faked" — see
	// migration 000018.
	var accrued money.Kobo
	if emp.IsHourly() {
		basis, err := s.hourlyMonthlyEarningsBasisTx(tx, orgID, employeeID, emp.HourlyRateKobo, asOf)
		if err != nil {
			return nil, err
		}
		el.MonthlySalary = basis
		accrued, err = s.accruedHourlyToDateTx(tx, orgID, employeeID, emp.HourlyRateKobo, period, asOf)
		if err != nil {
			return nil, err
		}
	} else {
		el.MonthlySalary = emp.Salary
		var err error
		accrued, err = AccruedToDate(emp.Salary, period, asOf)
		if err != nil {
			return nil, err
		}
	}
	el.AccruedToDate = accrued

	// periodEarningsBasis is "what lands on payday if nothing else changes",
	// used below for the protected-payday headroom and the projected-payday
	// figures. For salaried staff that is the fixed salary, known up front.
	// Hourly/gig workers have no such fixed figure until the period closes, so
	// this uses accrued-to-date instead: a running total that only reflects
	// hours actually approved, never a prediction of hours not yet worked. It
	// necessarily understates final pay early in the period and converges to
	// the true total as more entries are approved — the safe direction to be
	// wrong in, since it never promises a worker more than has been verified.
	periodEarningsBasis := emp.Salary
	if emp.IsHourly() {
		periodEarningsBasis = accrued
	}

	// Policy cap: a share of what is actually earned, then a hard ceiling.
	policyCap, err := accrued.Percent(int64(policy.MaxAccrualPct), 100)
	if err != nil {
		return nil, err
	}
	if policyCap > policy.AbsoluteCapKobo {
		policyCap = policy.AbsoluteCapKobo
	}
	el.PolicyCap = policyCap

	// Dependency assessment over the rolling window.
	history, err := s.drawHistoryTx(tx, orgID, employeeID, asOf)
	if err != nil {
		return nil, err
	}
	el.Dependency = ScoreDependency(DependencyInput{
		Now:           asOf,
		Draws:         history,
		MonthlySalary: el.MonthlySalary,
		LastPayday:    lastPayday(asOf),
	})

	el.TierCap = TierCap(el.Dependency.Tier, policyCap, *policy)

	// Outstanding advances for this period reduce what is left. "Outstanding"
	// (approved + disbursed) shrinks the cap, because an approved-but-undisbursed
	// advance is still a committed draw against this period's allowance.
	// "Recoverable" (disbursed only) is the narrower figure that actually funds
	// the payday projection below: SettleAdvancesForPayrollItem cancels an
	// approved advance that never disbursed and withholds nothing for it, so
	// counting it as a payday deduction would show the worker a smaller number
	// than payroll will actually pay.
	outstanding, recoverable, drawsThisPeriod, lastDrawAt, err :=
		s.outstandingTx(tx, orgID, employeeID, period)
	if err != nil {
		return nil, err
	}
	el.Outstanding = outstanding
	el.DrawsThisPeriod = drawsThisPeriod

	available, err := el.TierCap.Sub(outstanding)
	if err != nil || available.IsNegative() {
		available = money.Zero
	}

	// The worker's own floor caps everything else. Applied last and
	// unconditionally: a self-imposed limit must not be overridden by a policy
	// or tier that happens to be more generous, or it is not a limit at all.
	pref, err := s.workerPreferenceTx(tx, orgID, employeeID)
	if err != nil {
		return nil, err
	}
	el.ProtectedPayday = pref.ProtectedPayday()
	if el.ProtectedPayday.IsPositive() {
		spendable, subErr := periodEarningsBasis.Sub(el.ProtectedPayday)
		if subErr != nil || spendable.IsNegative() {
			spendable = money.Zero
		}
		headroom, subErr := spendable.Sub(outstanding)
		if subErr != nil || headroom.IsNegative() {
			headroom = money.Zero
		}
		if headroom < available {
			available = headroom
		}
	}

	// Employer funding coverage — opt-in per org (migration 000019). Applied
	// the same way as the worker's own floor: it can only narrow `available`,
	// never widen it. fundingBound is tracked separately so a decline caused
	// by an unfunded pool is reported as that, not as "you haven't earned
	// enough" — the worker did nothing wrong here.
	fundingBound := false
	if policy.RequireFundingCoverage {
		exposure, err := models.FundingExposure(tx, orgID, money.NGN)
		if err != nil {
			return nil, err
		}
		fundingHeadroom := money.Zero
		if exposure.Minor < 0 {
			fundingHeadroom = money.Kobo(-exposure.Minor)
		}
		if fundingHeadroom < available {
			available = fundingHeadroom
			fundingBound = true
		}
		el.FundingBound = fundingBound
	}
	el.Available = available

	// What actually lands on payday, now and in the worst case. Based on
	// `recoverable`, not `outstanding` — see the comment above outstandingTx.
	if projected, subErr := periodEarningsBasis.Sub(recoverable); subErr == nil && !projected.IsNegative() {
		el.ProjectedPayday = projected
		if worst, wErr := projected.Sub(available); wErr == nil && !worst.IsNegative() {
			el.ProjectedPaydayIfMaxDrawn = worst
		}
	}

	// Velocity guardrails.
	if drawsThisPeriod >= policy.MaxDrawsPerPeriod {
		el.Blocked, el.BlockedReason = true, DeclineDrawLimit
		next := firstOfNextPeriod(asOf)
		el.NextEligibleAt = &next
		return el, nil
	}
	if !lastDrawAt.IsZero() {
		wait := TierCoolingOff(el.Dependency.Tier, *policy)
		if ready := lastDrawAt.Add(wait); asOf.Before(ready) {
			el.Blocked, el.BlockedReason = true, DeclineCoolingOff
			el.NextEligibleAt = &ready
			return el, nil
		}
	}
	if available < policy.MinDrawKobo {
		el.Blocked = true
		if fundingBound {
			el.BlockedReason = DeclineFundingPoolExhausted
		} else {
			el.BlockedReason = DeclineExceedsEarned
		}
	}
	return el, nil
}

// RequestAdvance is the write path: it recomputes eligibility under the same
// transaction that writes the advance, so a concurrent request cannot slip past
// a cap that was checked a moment earlier.
func (s *EWAService) RequestAdvance(
	ctx context.Context, orgID, employeeID string, amount money.Kobo, idempotencyKey, actorIP string,
) (*models.EWAAdvance, *Eligibility, error) {
	var advance *models.EWAAdvance
	var eligibility *Eligibility

	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		// Replay of a previous request returns the original decision.
		if idempotencyKey != "" {
			var existing models.EWAAdvance
			err := tx.Where("organization_id = ? AND employee_id = ? AND idempotency_key = ?",
				orgID, employeeID, idempotencyKey).First(&existing).Error
			if err == nil {
				advance = &existing
				return nil
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}

		now := time.Now()
		el, err := s.eligibilityTx(tx, orgID, employeeID, now)
		if err != nil {
			return err
		}
		eligibility = el

		record := models.EWAAdvance{
			OrganizationID:         orgID,
			EmployeeID:             employeeID,
			Period:                 el.Period,
			AmountKobo:             amount,
			FeeKobo:                money.Zero, // fee-free by design; see docs/EWA_ROADMAP.md
			AccruedAtRequestKobo:   el.AccruedToDate,
			AvailableAtRequestKobo: el.Available,
			DependencyScore:        el.Dependency.Score,
			DependencyTier:         el.Dependency.Tier,
			RequestedAt:            now,
		}
		if idempotencyKey != "" {
			record.IdempotencyKey = &idempotencyKey
		}
		if uid, err := s.userIDForEmployee(tx, employeeID); err == nil && uid != "" {
			record.UserID = &uid
		}

		// Decide.
		reason := ""
		switch {
		case el.Blocked:
			reason = el.BlockedReason
		case amount < el.MinimumDraw:
			reason = DeclineBelowMinimum
		case amount > el.Available && el.FundingBound:
			// The funding pool, not the worker's own earnings, is what capped
			// Available below the requested amount — same attribution as the
			// el.Blocked case above, just below the MinimumDraw threshold that
			// trips Blocked. See the FundingBound doc comment.
			reason = DeclineFundingPoolExhausted
		case amount > el.Available:
			reason = DeclineExceedsEarned
		}

		if reason != "" {
			// Declines are recorded, not discarded: a pattern of declined requests
			// is one of the clearest signals that someone needs help rather than
			// another advance.
			record.Status = models.AdvanceDeclined
			record.DeclineReason = reason
			if err := tx.Create(&record).Error; err != nil {
				return err
			}
			advance = &record
			observability.EWAAdvancesTotal.WithLabelValues(orgID, "rejected").Inc()
			observability.EWADeclineReasonsTotal.WithLabelValues(orgID, reason).Inc()
			return models.AppendAuditTx(tx, orgID, "EWAAdvance", record.ID, "declined",
				"", reason, actorIP, "")
		}

		record.Status = models.AdvanceApproved
		if err := tx.Create(&record).Error; err != nil {
			return err
		}

		// The money movement and the record of it commit together or not at all.
		if err := s.postAdvanceLedger(tx, orgID, employeeID, &record); err != nil {
			return err
		}

		advance = &record
		observability.EWAAdvancesTotal.WithLabelValues(orgID, "approved").Inc()
		if el.AccruedToDate.IsPositive() {
			observability.EWAUtilizationRatio.WithLabelValues(orgID).
				Observe(float64(amount) / float64(el.AccruedToDate))
		}
		return models.AppendAuditTx(tx, orgID, "EWAAdvance", record.ID, "approved",
			"", amount.String(), actorIP, "")
	})
	if err != nil {
		return nil, eligibility, err
	}
	if advance != nil && advance.Status == models.AdvanceDeclined {
		return advance, eligibility, fmt.Errorf("%w: %s", ErrAdvanceDeclined, advance.DeclineReason)
	}

	// Enqueue after commit, gated on STATE rather than on "was this call the one
	// that created the approval". Gating on freshness alone would strand an
	// advance forever if the enqueue below ever failed: a client retrying the
	// exact same request hits the idempotency-key replay branch above and
	// returns early, never reaching this point again. Checking
	// ProviderReference instead means a retried request keeps re-attempting the
	// enqueue for as long as it's still unsubmitted — the retry IS the recovery
	// path, with no separate reconciliation mechanism required for this case.
	// The worker itself tolerates a duplicate enqueue: MarkSubmittedToProvider
	// is guarded on provider_reference being NULL, so two workers racing on the
	// same advance can both call the provider (relying on the provider's own
	// idempotency keying by our reference) but only one submission gets recorded.
	if advance != nil && advance.Status == models.AdvanceApproved && advance.ProviderReference == nil {
		payload, _ := json.Marshal(map[string]string{"advance_id": advance.ID, "org_id": orgID})
		task := asynq.NewTask(workers.TypeDisburseEWAAdvance, payload)
		if _, enqueueErr := workers.Client.Enqueue(task); enqueueErr != nil {
			observability.EWADisbursementEnqueueFailuresTotal.WithLabelValues(orgID).Inc()
		}
	}

	return advance, eligibility, nil
}

// postAdvanceLedger records the value movement: the worker is owed less future
// pay, and cash has left the funding source.
//
//	Dr advance_receivable (employee)   money we expect back from payroll
//	Cr cash_settlement    (org)        money that left
func (s *EWAService) postAdvanceLedger(tx *gorm.DB, orgID, employeeID string, adv *models.EWAAdvance) error {
	receivable, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
	if err != nil {
		return err
	}
	cash, err := models.EnsureAccount(tx, orgID, "", models.AccountCashSettlement, money.NGN)
	if err != nil {
		return err
	}

	_, err = models.PostTransaction(tx, models.PostingRequest{
		OrgID:          orgID,
		Kind:           "ewa_advance",
		Reference:      adv.ID,
		IdempotencyKey: "ewa_advance:" + adv.ID,
		Entries: []models.EntryInput{
			{AccountID: receivable.ID, Direction: models.Debit, Amount: money.NGNFromKobo(adv.AmountKobo)},
			{AccountID: cash.ID, Direction: models.Credit, Amount: money.NGNFromKobo(adv.AmountKobo)},
		},
	})
	if err != nil {
		observability.LedgerImbalanceTotal.WithLabelValues(orgID).Inc()
	}
	return err
}

// SettleAdvancesForPayrollItem nets a worker's outstanding advances out of one
// payroll item and records the recovery in the ledger. Returns the amount
// withheld, which never exceeds `available` — the item's gross pay. Called
// inside the payroll creation transaction.
//
// Only *disbursed* advances are recovered:
//
//	Dr wage_payable        (employee)  we owe the worker less
//	Cr advance_receivable  (employee)  the debt is cleared
//
// An advance that was approved but never disbursed is **cancelled**, not
// settled, and withholds nothing. Deducting pay for money the worker never
// received would be a wage theft bug, not an accounting detail. Its original
// receivable is reversed rather than edited, because ledger entries are
// immutable:
//
//	Dr cash_settlement     (org)       the committed cash was never sent
//	Cr advance_receivable  (employee)  the receivable is written back
//
// Disbursed advances older first (RequestedAt ascending) are recovered in
// full until `available` runs out. If a worker's outstanding advances exceed
// what this item can cover — several small draws stacked up, or a policy cap
// tightened after they were disbursed — the last advance touched is settled
// **partially**: only `available`'s remainder is withheld and recorded
// against RecoveredKobo, the advance stays Disbursed, and whatever is left
// waits for a future payroll run rather than either blocking this entire
// payroll batch or forcing net pay negative. Any advance still untouched
// after `available` reaches zero is left exactly as it was.
func (s *EWAService) SettleAdvancesForPayrollItem(
	tx *gorm.DB, orgID, employeeID, period, payrollItemID string, available money.Kobo,
) (money.Kobo, error) {
	var advances []models.EWAAdvance
	if err := tx.Where(
		"organization_id = ? AND employee_id = ? AND period = ? AND status IN ?",
		orgID, employeeID, period,
		[]models.AdvanceStatus{models.AdvanceApproved, models.AdvanceDisbursed},
	).Order("requested_at ASC").Find(&advances).Error; err != nil {
		return 0, err
	}
	if len(advances) == 0 {
		return money.Zero, nil
	}

	receivable, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountAdvanceReceivable, money.NGN)
	if err != nil {
		return 0, err
	}
	payable, err := models.EnsureAccount(tx, orgID, employeeID, models.AccountWagePayable, money.NGN)
	if err != nil {
		return 0, err
	}

	var withheld money.Kobo
	remaining := available
	for i := range advances {
		adv := &advances[i]

		if adv.Status == models.AdvanceApproved {
			// Committed but never disbursed by settlement time. Cancel it and
			// reverse the receivable; the worker gets their full pay. Shared with
			// the disbursement webhook's failure path — see CancelAdvance. No
			// cash moved, so this doesn't touch `remaining`.
			if err := models.CancelAdvance(tx, adv, models.AdvanceApproved, "settled_before_disbursement"); err != nil {
				if errors.Is(err, models.ErrStaleStatus) {
					continue
				}
				return 0, err
			}
			continue
		}

		owed := adv.RemainingKobo()
		if !owed.IsPositive() {
			continue // fully recovered by an earlier partial pass
		}
		portion := owed
		if remaining < owed {
			portion = remaining
		}
		if !portion.IsPositive() {
			continue // this item's net pay is already exhausted; later advances wait for a future run
		}

		if _, err := models.PostTransaction(tx, models.PostingRequest{
			OrgID:     orgID,
			Kind:      "ewa_settlement",
			Reference: adv.ID,
			// Keyed per (advance, payroll item), not just per advance: a
			// partially recovered advance is legitimately posted against more
			// than one payroll item over time, and each such installment must
			// get its own idempotency key while a retry of the same item stays
			// a no-op.
			IdempotencyKey: "ewa_settlement:" + adv.ID + ":" + payrollItemID,
			Entries: []models.EntryInput{
				{AccountID: payable.ID, Direction: models.Debit, Amount: money.NGNFromKobo(portion)},
				{AccountID: receivable.ID, Direction: models.Credit, Amount: money.NGNFromKobo(portion)},
			},
		}); err != nil {
			observability.LedgerImbalanceTotal.WithLabelValues(orgID).Inc()
			return 0, err
		}

		recoveredTotal, addErr := adv.RecoveredKobo.Add(portion)
		if addErr != nil {
			return 0, fmt.Errorf("recovered total overflow: %w", addErr)
		}
		res := tx.Model(&models.EWAAdvance{}).
			Where("id = ? AND recovered_kobo = ?", adv.ID, adv.RecoveredKobo).
			Update("recovered_kobo", recoveredTotal)
		if res.Error != nil {
			return 0, res.Error
		}
		if res.RowsAffected == 0 {
			return 0, fmt.Errorf("%w: recovered_kobo changed concurrently for %s", models.ErrStaleStatus, adv.ID)
		}
		adv.RecoveredKobo = recoveredTotal

		if recoveredTotal >= adv.AmountKobo {
			if err := models.TransitionAdvance(tx, adv, models.AdvanceDisbursed, models.AdvanceSettled); err != nil {
				if !errors.Is(err, models.ErrStaleStatus) {
					return 0, err
				}
			} else if err := tx.Model(&models.EWAAdvance{}).Where("id = ?", adv.ID).
				Update("settled_payroll_item_id", payrollItemID).Error; err != nil {
				return 0, err
			}
		} else {
			if err := models.AppendAuditTx(tx, orgID, "EWAAdvance", adv.ID, "partially_settled",
				portion.String(), fmt.Sprintf("remaining=%s payroll_item=%s", adv.RemainingKobo(), payrollItemID),
				"internal", ""); err != nil {
				return 0, err
			}
		}

		next, err := withheld.Add(portion)
		if err != nil {
			return 0, fmt.Errorf("settlement total overflow: %w", err)
		}
		withheld = next

		remaining, err = remaining.Sub(portion)
		if err != nil {
			return 0, fmt.Errorf("available cap underflow: %w", err)
		}
	}
	return withheld, nil
}

// workerPreferenceTx loads the worker's self-imposed floor, defaulting to none.
func (s *EWAService) workerPreferenceTx(
	tx *gorm.DB, orgID, employeeID string,
) (*models.EWAWorkerPreference, error) {
	var pref models.EWAWorkerPreference
	err := tx.First(&pref, "organization_id = ? AND employee_id = ?", orgID, employeeID).Error
	if err == nil {
		return &pref, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	return &models.EWAWorkerPreference{OrganizationID: orgID, EmployeeID: employeeID}, nil
}

// ErrFloorLoweringRateLimited is returned when a worker tries to lower their
// protected-payday floor before the cooling-off period has elapsed.
var ErrFloorLoweringRateLimited = errors.New("floor lowering is rate-limited")

// SetProtectedPayday sets or updates the worker's self-imposed floor.
//
// Raising it (or setting it for the first time) takes effect immediately —
// protecting more of payday is always allowed. Lowering it is rate-limited by
// the org's cooling-off policy, the same one that governs draws: without that,
// a worker could drop their own floor in the exact moment they are tempted to
// draw past it, and the floor would be decorative rather than binding. It is
// applied by their own choice, but held to it by the system, which is the
// entire point of a commitment device.
func (s *EWAService) SetProtectedPayday(
	ctx context.Context, orgID, employeeID string, amount money.Kobo, now time.Time,
) (*models.EWAWorkerPreference, error) {
	var result *models.EWAWorkerPreference
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		current, err := s.workerPreferenceTx(tx, orgID, employeeID)
		if err != nil {
			return err
		}
		policy, err := s.policyTx(tx, orgID)
		if err != nil {
			return err
		}

		// Gated against LastChangedAt, not "last time it was lowered": with the
		// latter, the very first lowering after a raise would have nothing to
		// rate-limit against and would slip through unprotected.
		lowering := amount < current.ProtectedPayday()
		if lowering && current.LastChangedAt != nil {
			wait := time.Duration(policy.CoolingOffHours) * time.Hour
			if ready := current.LastChangedAt.Add(wait); now.Before(ready) {
				return fmt.Errorf("%w: try again after %s", ErrFloorLoweringRateLimited, ready.Format(time.RFC3339))
			}
		}

		updated := models.EWAWorkerPreference{
			OrganizationID:       orgID,
			EmployeeID:           employeeID,
			ProtectedPaydayMinor: int64(amount),
			LastChangedAt:        &now,
		}

		// An explicit upsert, not tx.Save. The primary key is composite
		// (org_id, employee_id); GORM's Save() issues an UPDATE keyed on a
		// non-zero primary key and does NOT fall back to INSERT when no row
		// matches, so on a worker's first-ever call it would silently affect
		// zero rows — the preference would never be persisted, and every
		// GetEligibility call afterward would keep reading the zero default.
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "organization_id"}, {Name: "employee_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"protected_payday_minor", "last_changed_at", "updated_at"}),
		}).Create(&updated).Error; err != nil {
			return err
		}
		result = &updated
		return nil
	})
	return result, err
}

// policyTx loads the org's policy, falling back to the conservative default.
func (s *EWAService) policyTx(tx *gorm.DB, orgID string) (*models.EWAPolicy, error) {
	var policy models.EWAPolicy
	err := tx.First(&policy, "organization_id = ?", orgID).Error
	if err == nil {
		return &policy, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	def := models.DefaultEWAPolicy(orgID)
	return &def, nil
}

// drawHistoryTx loads advances inside the dependency window.
func (s *EWAService) drawHistoryTx(tx *gorm.DB, orgID, employeeID string, asOf time.Time) ([]DrawEvent, error) {
	var rows []models.EWAAdvance
	if err := tx.Where(
		"organization_id = ? AND employee_id = ? AND requested_at >= ? AND status IN ?",
		orgID, employeeID, asOf.Add(-dependencyWindow),
		[]models.AdvanceStatus{
			models.AdvanceApproved, models.AdvanceDisbursed, models.AdvanceSettled,
		},
	).Find(&rows).Error; err != nil {
		return nil, err
	}

	events := make([]DrawEvent, 0, len(rows))
	for _, r := range rows {
		events = append(events, DrawEvent{At: r.RequestedAt, Amount: r.AmountKobo})
	}
	return events, nil
}

// outstandingTx sums unsettled advances for the period and reports draw count
// and the most recent draw time, which drive the velocity guardrails.
//
// Two totals are returned because they answer different questions:
//
//   - outstanding (approved + disbursed) is what still counts against this
//     period's allowance. An approved-but-undisbursed advance is a committed
//     draw the worker cannot request again, so it must shrink the cap.
//   - recoverable (disbursed only) is what payroll will actually deduct.
//     SettleAdvancesForPayrollItem cancels an approved advance that never
//     disbursed rather than withholding it — see that function's comment — so
//     an approved-but-undisbursed advance must NOT reduce the payday
//     projection or it tells the worker a smaller number than they will
//     actually be paid.
func (s *EWAService) outstandingTx(
	tx *gorm.DB, orgID, employeeID, period string,
) (outstanding, recoverable money.Kobo, drawCount int, lastDrawAt time.Time, err error) {
	var rows []models.EWAAdvance
	if err := tx.Where(
		"organization_id = ? AND employee_id = ? AND period = ? AND status IN ?",
		orgID, employeeID, period,
		[]models.AdvanceStatus{
			models.AdvanceApproved, models.AdvanceDisbursed, models.AdvanceSettled,
		},
	).Find(&rows).Error; err != nil {
		return 0, 0, 0, time.Time{}, err
	}

	for _, r := range rows {
		if r.IsOutstanding() {
			// RemainingKobo, not AmountKobo: a partially settled advance
			// (SettleAdvancesForPayrollItem could only cover part of it from a
			// prior payroll run) has already given up some of what it owed, and
			// must not keep consuming the full original amount against this
			// period's draw cap or payday projection.
			next, addErr := outstanding.Add(r.RemainingKobo())
			if addErr != nil {
				return 0, 0, 0, time.Time{}, fmt.Errorf("outstanding total overflow: %w", addErr)
			}
			outstanding = next

			if r.Status == models.AdvanceDisbursed {
				next, addErr := recoverable.Add(r.RemainingKobo())
				if addErr != nil {
					return 0, 0, 0, time.Time{}, fmt.Errorf("recoverable total overflow: %w", addErr)
				}
				recoverable = next
			}
		}
		if r.RequestedAt.After(lastDrawAt) {
			lastDrawAt = r.RequestedAt
		}
	}
	return outstanding, recoverable, len(rows), lastDrawAt, nil
}

// userIDForEmployee resolves the worker's app identity. The advance references
// it when one exists; a missing User must not block a draw.
func (s *EWAService) userIDForEmployee(tx *gorm.DB, employeeID string) (string, error) {
	var user models.User
	if err := tx.Select("id").First(&user, "employee_id = ?", employeeID).Error; err != nil {
		return "", err
	}
	return user.ID, nil
}

// lastPayday assumes month-end pay: the last day of the previous period.
// Replaced by the org's real pay calendar when that exists.
func lastPayday(asOf time.Time) time.Time {
	return time.Date(asOf.Year(), asOf.Month(), 1, 0, 0, 0, 0, asOf.Location()).AddDate(0, 0, -1)
}

// firstOfNextPeriod is when a period-scoped limit resets.
func firstOfNextPeriod(asOf time.Time) time.Time {
	return time.Date(asOf.Year(), asOf.Month(), 1, 0, 0, 0, 0, asOf.Location()).AddDate(0, 1, 0)
}
