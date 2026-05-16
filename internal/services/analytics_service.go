package services

import (
	"fmt"
	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/repository"
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
	PredictedAmount float64 `json:"predicted_amount"`
	CurrentBalance  float64 `json:"current_balance"`
	RiskLevel       string  `json:"risk_level"`
	Message         string  `json:"message"`
}

// GetPredictiveCashFlow — weighted sliding window forecast vs live wallet balance.
// Weights [3,2,1] bias toward recent payrolls to catch salary growth trends early.
func (s *AnalyticsService) GetPredictiveCashFlow(orgID string) (*PredictionResult, error) {
	payrolls, err := s.payrollRepo.FindCompleted(orgID, 3)
	if err != nil {
		return nil, err
	}

	var predictedAmount float64
	if len(payrolls) == 0 {
		// Cold-start: no history — sum active salaries as a baseline.
		employees, err := s.employeeRepo.FindAllActive(orgID)
		if err != nil {
			return nil, err
		}
		for _, e := range employees {
			predictedAmount += e.Salary
		}
	} else {
		// Weighted sliding window: most recent = weight 3, second = 2, oldest = 1.
		weights := []float64{3, 2, 1}
		var weightedSum, totalWeight float64
		for i, p := range payrolls {
			w := weights[i]
			weightedSum += p.TotalAmount * w
			totalWeight += w
		}
		predictedAmount = weightedSum / totalWeight
	}

	walletNumber := os.Getenv("MONNIFY_SOURCE_WALLET")
	currentBalance, err := s.MonnifyClient.GetWalletBalance(walletNumber)
	if err != nil {
		return nil, fmt.Errorf("could not fetch balance: %v", err)
	}

	riskLevel := "Low"
	message := "Your balance is sufficient for the next payroll cycle."
	if currentBalance < predictedAmount {
		riskLevel = "High"
		message = fmt.Sprintf("Warning: Your balance (%.2f) is less than the predicted payroll amount (%.2f). Please fund your wallet.", currentBalance, predictedAmount)
	} else if currentBalance < predictedAmount*1.2 {
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
