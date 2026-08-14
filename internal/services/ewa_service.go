package services

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/observability"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
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
)

// ErrAdvanceDeclined is returned when a request fails a guardrail. The advance
// row is still written with status=declined so the decision is auditable.
var ErrAdvanceDeclined = errors.New("advance declined")

// EWAService owns earned wage access: what a worker has earned, what they may
// draw, and what happens when they draw it.
type EWAService struct{}

// NewEWAService constructs the service.
func NewEWAService() *EWAService { return &EWAService{} }

// AccruedToDate returns wages earned so far in the period.
//
// Accrual is straight-line across *working* days (Mon–Fri), not calendar days:
// a worker three days into a month has not earned 10% of a monthly salary, and
// paying as if they had is how an EWA product ends up advancing unearned wages.
// Salaried staff on a monthly cycle is the assumption; hourly and shift-based
// accrual needs real timesheet data and is deliberately out of scope here rather
// than approximated.
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

	DrawsThisPeriod int        `json:"draws_this_period"`
	MaxDraws        int        `json:"max_draws_per_period"`
	NextEligibleAt  *time.Time `json:"next_eligible_at,omitempty"`

	Dependency DependencyAssessment `json:"dependency"`
	// Blocked is set when a guardrail currently prevents any draw.
	Blocked       bool   `json:"blocked"`
	BlockedReason string `json:"blocked_reason,omitempty"`
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
		EmployeeID:    emp.ID,
		Period:        period,
		MonthlySalary: emp.Salary,
		MinimumDraw:   policy.MinDrawKobo,
		MaxDraws:      policy.MaxDrawsPerPeriod,
	}

	if !policy.Enabled {
		el.Blocked, el.BlockedReason = true, DeclineEWADisabled
		return el, nil
	}
	if !emp.IsActive {
		el.Blocked, el.BlockedReason = true, DeclineInactiveAccount
		return el, nil
	}
	if !emp.Salary.IsPositive() {
		el.Blocked, el.BlockedReason = true, DeclineNoSalaryOnFile
		return el, nil
	}

	accrued, err := AccruedToDate(emp.Salary, period, asOf)
	if err != nil {
		return nil, err
	}
	el.AccruedToDate = accrued

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
		MonthlySalary: emp.Salary,
		LastPayday:    lastPayday(asOf),
	})

	el.TierCap = TierCap(el.Dependency.Tier, policyCap, *policy)

	// Outstanding advances for this period reduce what is left.
	outstanding, drawsThisPeriod, lastDrawAt, err := s.outstandingTx(tx, orgID, employeeID, period)
	if err != nil {
		return nil, err
	}
	el.Outstanding = outstanding
	el.DrawsThisPeriod = drawsThisPeriod

	available, err := el.TierCap.Sub(outstanding)
	if err != nil || available.IsNegative() {
		available = money.Zero
	}
	el.Available = available

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
		el.Blocked, el.BlockedReason = true, DeclineExceedsEarned
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
// withheld. Called inside the payroll creation transaction.
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
func (s *EWAService) SettleAdvancesForPayrollItem(
	tx *gorm.DB, orgID, employeeID, period, payrollItemID string,
) (money.Kobo, error) {
	var advances []models.EWAAdvance
	if err := tx.Where(
		"organization_id = ? AND employee_id = ? AND period = ? AND status IN ?",
		orgID, employeeID, period,
		[]models.AdvanceStatus{models.AdvanceApproved, models.AdvanceDisbursed},
	).Find(&advances).Error; err != nil {
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

	cash, err := models.EnsureAccount(tx, orgID, "", models.AccountCashSettlement, money.NGN)
	if err != nil {
		return 0, err
	}

	var withheld money.Kobo
	for i := range advances {
		adv := &advances[i]

		if adv.Status == models.AdvanceApproved {
			// Committed but never sent. Cancel it and reverse the receivable; the
			// worker gets their full pay.
			if err := models.TransitionAdvance(tx, adv, models.AdvanceApproved, models.AdvanceCancelled); err != nil {
				if errors.Is(err, models.ErrStaleStatus) {
					continue
				}
				return 0, err
			}
			if _, err := models.PostTransaction(tx, models.PostingRequest{
				OrgID:          orgID,
				Kind:           "ewa_cancellation",
				Reference:      adv.ID,
				IdempotencyKey: "ewa_cancellation:" + adv.ID,
				Entries: []models.EntryInput{
					{AccountID: cash.ID, Direction: models.Debit, Amount: money.NGNFromKobo(adv.AmountKobo)},
					{AccountID: receivable.ID, Direction: models.Credit, Amount: money.NGNFromKobo(adv.AmountKobo)},
				},
			}); err != nil {
				observability.LedgerImbalanceTotal.WithLabelValues(orgID).Inc()
				return 0, err
			}
			continue
		}

		if err := models.TransitionAdvance(tx, adv, models.AdvanceDisbursed, models.AdvanceSettled); err != nil {
			if errors.Is(err, models.ErrStaleStatus) {
				continue // another settlement pass won
			}
			return 0, err
		}
		if err := tx.Model(&models.EWAAdvance{}).Where("id = ?", adv.ID).
			Update("settled_payroll_item_id", payrollItemID).Error; err != nil {
			return 0, err
		}

		if _, err := models.PostTransaction(tx, models.PostingRequest{
			OrgID:          orgID,
			Kind:           "ewa_settlement",
			Reference:      adv.ID,
			IdempotencyKey: "ewa_settlement:" + adv.ID,
			Entries: []models.EntryInput{
				{AccountID: payable.ID, Direction: models.Debit, Amount: money.NGNFromKobo(adv.AmountKobo)},
				{AccountID: receivable.ID, Direction: models.Credit, Amount: money.NGNFromKobo(adv.AmountKobo)},
			},
		}); err != nil {
			observability.LedgerImbalanceTotal.WithLabelValues(orgID).Inc()
			return 0, err
		}

		next, err := withheld.Add(adv.AmountKobo)
		if err != nil {
			return 0, fmt.Errorf("settlement total overflow: %w", err)
		}
		withheld = next
	}
	return withheld, nil
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
func (s *EWAService) outstandingTx(
	tx *gorm.DB, orgID, employeeID, period string,
) (money.Kobo, int, time.Time, error) {
	var rows []models.EWAAdvance
	if err := tx.Where(
		"organization_id = ? AND employee_id = ? AND period = ? AND status IN ?",
		orgID, employeeID, period,
		[]models.AdvanceStatus{
			models.AdvanceApproved, models.AdvanceDisbursed, models.AdvanceSettled,
		},
	).Find(&rows).Error; err != nil {
		return 0, 0, time.Time{}, err
	}

	var outstanding money.Kobo
	var last time.Time
	for _, r := range rows {
		if r.IsOutstanding() {
			next, err := outstanding.Add(r.AmountKobo)
			if err != nil {
				return 0, 0, time.Time{}, fmt.Errorf("outstanding total overflow: %w", err)
			}
			outstanding = next
		}
		if r.RequestedAt.After(last) {
			last = r.RequestedAt
		}
	}
	return outstanding, len(rows), last, nil
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
