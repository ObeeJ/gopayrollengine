package services

import (
	"fmt"
	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/models"
	"os"
)

type AnalyticsService struct {
	MonnifyClient *monnify.Client
}

func NewAnalyticsService() *AnalyticsService {
	return &AnalyticsService{
		MonnifyClient: monnify.NewClient(),
	}
}

type PredictionResult struct {
	PredictedAmount float64 `json:"predicted_amount"`
	CurrentBalance  float64 `json:"current_balance"`
	RiskLevel       string  `json:"risk_level"`
	Message         string  `json:"message"`
}

// GetPredictiveCashFlow calculates the expected cost of the next payroll cycle
// and compares it against the live Monnify wallet balance to produce a risk assessment.
//
// Prediction logic:
//   - If completed payroll history exists: weighted sliding window average of last 3
//     batches. Weights [3,2,1] (most recent weighted highest) detect salary growth
//     trends that a simple average would underestimate.
//   - If no history: sum current active employee salaries as a cold-start baseline.
//
// Risk levels:
//   - High   → balance < predicted amount (cannot cover payroll).
//   - Medium → balance < predicted amount * 1.2 (less than 20% buffer).
//   - Low    → balance is comfortably above the predicted amount.
func (s *AnalyticsService) GetPredictiveCashFlow() (*PredictionResult, error) {
	var payrolls []models.Payroll
	// Fetch the most recent 3 completed batches ordered newest-first.
	if err := models.DB.Where("status = ?", models.PayrollCompleted).Order("created_at desc").Limit(3).Find(&payrolls).Error; err != nil {
		return nil, err
	}

	var predictedAmount float64
	if len(payrolls) == 0 {
		// Cold-start: no history yet — sum active salaries as a baseline estimate.
		var employees []models.Employee
		models.DB.Where("is_active = ?", true).Find(&employees)
		for _, e := range employees {
			predictedAmount += e.Salary
		}
	} else {
		// Weighted sliding window: payrolls[0] is most recent (weight 3),
		// payrolls[1] is second (weight 2), payrolls[2] is oldest (weight 1).
		// This biases the forecast toward recent salary changes rather than
		// treating all months equally, giving a more accurate buffer warning.
		weights := []float64{3, 2, 1}
		var weightedSum, totalWeight float64
		for i, p := range payrolls {
			w := weights[i]
			weightedSum += p.TotalAmount * w
			totalWeight += w
		}
		predictedAmount = weightedSum / totalWeight
	}

	// Fetch the live wallet balance from Monnify to compare against the forecast.
	walletNumber := os.Getenv("MONNIFY_SOURCE_WALLET")
	currentBalance, err := s.MonnifyClient.GetWalletBalance(walletNumber)
	if err != nil {
		return nil, fmt.Errorf("could not fetch balance: %v", err)
	}

	// Determine risk level based on headroom above the predicted cost.
	riskLevel := "Low"
	message := "Your balance is sufficient for the next payroll cycle."

	if currentBalance < predictedAmount {
		// Balance cannot cover the next payroll — immediate action required.
		riskLevel = "High"
		message = fmt.Sprintf("Warning: Your balance (%.2f) is less than the predicted payroll amount (%.2f). Please fund your wallet.", currentBalance, predictedAmount)
	} else if currentBalance < predictedAmount*1.2 {
		// Less than 20% buffer — worth topping up before the next cycle.
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
