package services

import (
	"context"
	"fmt"
	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/pkg/money"
	"os"

	"gorm.io/gorm"
)

type AnalyticsService struct {
	MonnifyClient *monnify.Client
	payrollRepo   repository.PayrollRepository
	employeeRepo  repository.EmployeeRepository
}

// NewAnalyticsService — wires up analytics with its dependencies.
func NewAnalyticsService(pr repository.PayrollRepository, er repository.EmployeeRepository) *AnalyticsService {
	return &AnalyticsService{
		MonnifyClient: monnify.NewClient(),
		payrollRepo:   pr,
		employeeRepo:  er,
	}
}

type PredictionResult struct {
	PredictedAmount money.Kobo `json:"predicted_amount"`
	// CurrentBalance is the platform's shared Monnify source wallet — every
	// tenant's pooled payroll float, not this org's own money — so it drives
	// the risk level but is never serialized to a tenant.
	CurrentBalance money.Kobo `json:"-"`
	RiskLevel      string     `json:"risk_level"`
	Message        string     `json:"message"`
}

// GetPredictiveCashFlow — weighted sliding-window forecast vs live wallet balance; integer Kobo throughout.
//
// Both reads run inside WithOrgScope: payrolls and employees have forced
// row-level security, and read unscoped they return zero rows under the
// production database role — this always reported an empty history and a
// zero-employee cold start there, while tests run as a superuser (which
// bypasses RLS) saw real data. See
// TestPredictiveCashFlow_ReadsHistoryUnderProductionRole.
func (s *AnalyticsService) GetPredictiveCashFlow(ctx context.Context, orgID string) (*PredictionResult, error) {
	var payrolls []models.Payroll
	var employees []models.Employee
	if err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		var err error
		payrolls, err = s.payrollRepo.WithTx(tx).FindCompleted(orgID, 3)
		if err != nil || len(payrolls) > 0 {
			return err
		}
		employees, err = s.employeeRepo.WithTx(tx).FindAllActive(orgID)
		return err
	}); err != nil {
		return nil, err
	}

	var predictedAmount money.Kobo
	var err error
	if len(payrolls) == 0 {
		// Cold-start: no history — sum active salaries as a baseline.
		salaries := make([]money.Kobo, len(employees))
		for i, e := range employees {
			salaries[i] = e.Salary
		}
		predictedAmount, err = money.Sum(salaries)
		if err != nil {
			return nil, fmt.Errorf("cold-start prediction overflow: %w", err)
		}
	} else {
		// Weights [3,2,1] bias toward recent payrolls; overflow-checked integer math.
		weights := []int64{3, 2, 1}
		var weightedSum money.Kobo
		var totalWeight int64
		for i, p := range payrolls {
			if i >= len(weights) {
				break
			}
			scaled, err := p.TotalAmount.MulInt(weights[i])
			if err != nil {
				return nil, fmt.Errorf("weighted prediction overflow: %w", err)
			}
			weightedSum, err = weightedSum.Add(scaled)
			if err != nil {
				return nil, fmt.Errorf("weighted prediction overflow: %w", err)
			}
			totalWeight += weights[i]
		}
		predictedAmount, err = weightedSum.Percent(1, totalWeight)
		if err != nil {
			return nil, fmt.Errorf("weighted prediction division: %w", err)
		}
	}

	walletNumber := os.Getenv("MONNIFY_SOURCE_WALLET")
	currentBalance, err := s.MonnifyClient.GetWalletBalance(walletNumber)
	if err != nil {
		return nil, fmt.Errorf("could not fetch balance: %w", err)
	}

	// 120% threshold for "Medium" risk — banker's-rounded integer multiply.
	mediumThreshold, err := predictedAmount.Percent(120, 100)
	if err != nil {
		return nil, fmt.Errorf("threshold calc: %w", err)
	}

	riskLevel := "Low"
	message := "Your balance is sufficient for the next payroll cycle."
	switch {
	case currentBalance < predictedAmount:
		riskLevel = "High"
		message = fmt.Sprintf("Warning: available payroll funds are below the predicted payroll amount (%s). Please fund your wallet.", predictedAmount)
	case currentBalance < mediumThreshold:
		riskLevel = "Medium"
		message = "Your balance is close to the predicted payroll amount. Consider adding more funds."
	}

	return &PredictionResult{
		PredictedAmount: predictedAmount,
		CurrentBalance:  currentBalance,
		RiskLevel:       riskLevel,
		Message:         message,
	}, nil
}
