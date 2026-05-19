// Package services_test contains unit tests for the analytics and webhook
// reconciliation logic. Local helpers (computeRisk, resolvePayrollStatus)
// mirror the production logic so they can be exercised without a DB or Monnify.
package services

import (
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPredictiveAnalytics_AverageCalculation(t *testing.T) {
	payrolls := []models.Payroll{
		{TotalAmount: money.FromNaira(1000)},
		{TotalAmount: money.FromNaira(2000)},
		{TotalAmount: money.FromNaira(3000)},
	}

	var sum money.Kobo
	for _, p := range payrolls {
		next, err := sum.Add(p.TotalAmount)
		assert.NoError(t, err)
		sum = next
	}
	avg, err := sum.Percent(1, int64(len(payrolls)))
	assert.NoError(t, err)

	assert.Equal(t, money.FromNaira(2000), avg)
}

func TestRiskLevel_Low(t *testing.T) {
	balance := money.FromNaira(3000)
	predicted := money.FromNaira(2000)
	assert.Equal(t, "Low", computeRisk(balance, predicted))
}

func TestRiskLevel_Medium(t *testing.T) {
	// balance is between predicted and predicted*1.2
	balance := money.FromNaira(2300)
	predicted := money.FromNaira(2000)
	assert.Equal(t, "Medium", computeRisk(balance, predicted))
}

func TestRiskLevel_High(t *testing.T) {
	balance := money.FromNaira(1500)
	predicted := money.FromNaira(2000)
	assert.Equal(t, "High", computeRisk(balance, predicted))
}

func TestRiskLevel_ExactlyAtPredicted(t *testing.T) {
	// balance == predicted → Medium (not below predicted, but below predicted*1.2)
	balance := money.FromNaira(2000)
	predicted := money.FromNaira(2000)
	assert.Equal(t, "Medium", computeRisk(balance, predicted))
}

func TestRiskLevel_ExactlyAt120Percent(t *testing.T) {
	// balance == predicted*1.2 → Low (just above the medium threshold)
	balance := money.FromNaira(2400)
	predicted := money.FromNaira(2000)
	assert.Equal(t, "Low", computeRisk(balance, predicted))
}

func TestPredictiveAnalytics_NoHistory_UsesEmployeeSalaries(t *testing.T) {
	employees := []models.Employee{
		{Salary: money.FromNaira(100000)},
		{Salary: money.FromNaira(200000)},
		{Salary: money.FromNaira(150000)},
	}

	salaries := make([]money.Kobo, len(employees))
	for i, e := range employees {
		salaries[i] = e.Salary
	}
	total, err := money.Sum(salaries)
	assert.NoError(t, err)

	assert.Equal(t, money.FromNaira(450000), total)
}

func TestWebhookReconciliation_AllCompleted(t *testing.T) {
	items := []models.PayrollItem{
		{Status: models.PayrollCompleted},
		{Status: models.PayrollCompleted},
		{Status: models.PayrollCompleted},
	}
	assert.Equal(t, models.PayrollCompleted, resolvePayrollStatus(items))
}

func TestWebhookReconciliation_AnyFailed(t *testing.T) {
	items := []models.PayrollItem{
		{Status: models.PayrollCompleted},
		{Status: models.PayrollFailed},
		{Status: models.PayrollCompleted},
	}
	assert.Equal(t, models.PayrollFailed, resolvePayrollStatus(items))
}

func TestWebhookReconciliation_StillPending(t *testing.T) {
	items := []models.PayrollItem{
		{Status: models.PayrollCompleted},
		{Status: models.PayrollPending},
	}
	// Should not resolve yet — pending items remain
	assert.Equal(t, models.PayrollStatus(""), resolvePayrollStatus(items))
}

// computeRisk mirrors the logic in AnalyticsService.GetPredictiveCashFlow
// extracted here so it can be unit-tested without a DB or Monnify dependency.
// All arithmetic is integer Kobo with banker's rounded ×1.2 threshold.
func computeRisk(balance, predicted money.Kobo) string {
	if balance < predicted {
		return "High"
	}
	mediumThreshold, err := predicted.Percent(120, 100)
	if err != nil {
		return "High"
	}
	if balance < mediumThreshold {
		return "Medium"
	}
	return "Low"
}

// resolvePayrollStatus mirrors the reconciliation logic in webhook_handler.reconcilePayrollStatus.
// Returns "" if items are still pending (no status change yet).
func resolvePayrollStatus(items []models.PayrollItem) models.PayrollStatus {
	var pending, failed int
	for _, item := range items {
		switch item.Status {
		case models.PayrollPending, models.PayrollProcessing:
			pending++
		case models.PayrollFailed:
			failed++
		}
	}
	if pending > 0 {
		return ""
	}
	if failed > 0 {
		return models.PayrollFailed
	}
	return models.PayrollCompleted
}
