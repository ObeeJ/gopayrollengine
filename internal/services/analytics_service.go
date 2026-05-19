package services

import (
	"fmt"
	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/repository"
	"go-payroll-engine/pkg/money"
	"os"
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
	CurrentBalance  money.Kobo `json:"current_balance"`
	RiskLevel       string     `json:"risk_level"`
	Message         string     `json:"message"`
}

// GetPredictiveCashFlow — weighted sliding window forecast vs live wallet balance.
// Weights [3,2,1] bias toward recent payrolls to catch salary growth trends early.
// All arithmetic is integer Kobo; the only float comes from the Monnify wallet
// balance response and is converted at the client boundary.
func (s *AnalyticsService) GetPredictiveCashFlow(orgID string) (*PredictionResult, error) {
	payrolls, err := s.payrollRepo.FindCompleted(orgID, 3)
	if err != nil {
		return nil, err
	}

	var predictedAmount money.Kobo
	if len(payrolls) == 0 {
		// Cold-start: no history — sum active salaries as a baseline.
		employees, err := s.employeeRepo.FindAllActive(orgID)
		if err != nil {
			return nil, err
		}
		salaries := make([]money.Kobo, len(employees))
		for i, e := range employees {
			salaries[i] = e.Salary
		}
		predictedAmount, err = money.Sum(salaries)
		if err != nil {
			return nil, fmt.Errorf("cold-start prediction overflow: %w", err)
		}
	} else {
		// Weighted sliding window: most recent = weight 3, second = 2, oldest = 1.
		// Computed as Σ(amount × weight) / Σ(weight) using overflow-checked
		// integer arithmetic with banker's rounding on the final division.
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
		return nil, fmt.Errorf("could not fetch balance: %v", err)
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
		message = fmt.Sprintf("Warning: Your balance (%s) is less than the predicted payroll amount (%s). Please fund your wallet.", currentBalance, predictedAmount)
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
