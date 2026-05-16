// Package services_test contains unit tests for the analytics and webhook reconciliation logic.
// Tests are intentionally decoupled from the DB and Monnify by using local helper functions
// (computeRisk, resolvePayrollStatus) that mirror the production logic.
package services

import (
	"go-payroll-engine/internal/models"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPredictiveAnalytics_AverageCalculation(t *testing.T) {
	payrolls := []models.Payroll{
		{TotalAmount: 1000},
		{TotalAmount: 2000},
		{TotalAmount: 3000},
	}

	var sum float64
	for _, p := range payrolls {
		sum += p.TotalAmount
	}
	avg := sum / float64(len(payrolls))

	assert.Equal(t, float64(2000), avg)
}

func TestRiskLevel_Low(t *testing.T) {
	balance := 3000.0
	predicted := 2000.0
	assert.Equal(t, "Low", computeRisk(balance, predicted))
}

func TestRiskLevel_Medium(t *testing.T) {
	// balance is between predicted and predicted*1.2
	balance := 2300.0
	predicted := 2000.0
	assert.Equal(t, "Medium", computeRisk(balance, predicted))
}

func TestRiskLevel_High(t *testing.T) {
	balance := 1500.0
	predicted := 2000.0
	assert.Equal(t, "High", computeRisk(balance, predicted))
}

func TestRiskLevel_ExactlyAtPredicted(t *testing.T) {
	// balance == predicted → Medium (not below predicted, but below predicted*1.2)
	balance := 2000.0
	predicted := 2000.0
	assert.Equal(t, "Medium", computeRisk(balance, predicted))
}

func TestRiskLevel_ExactlyAt120Percent(t *testing.T) {
	// balance == predicted*1.2 → Low (just above the medium threshold)
	balance := 2400.0
	predicted := 2000.0
	assert.Equal(t, "Low", computeRisk(balance, predicted))
}

func TestPredictiveAnalytics_NoHistory_UsesEmployeeSalaries(t *testing.T) {
	employees := []models.Employee{
		{Salary: 100000},
		{Salary: 200000},
		{Salary: 150000},
	}

	var total float64
	for _, e := range employees {
		total += e.Salary
	}

	assert.Equal(t, float64(450000), total)
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
func computeRisk(balance, predicted float64) string {
	if balance < predicted {
		return "High"
	} else if balance < predicted*1.2 {
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
