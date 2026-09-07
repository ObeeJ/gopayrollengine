package models

import (
	"time"

	"go-payroll-engine/pkg/money"
)

// ReconciliationRun is one comparison of the ledger's implied cash position
// against the Monnify disbursement wallet's actual balance — see migration
// 000021 and internal/services/reconciliation_job.go for what's compared and
// why. System-level: no organization_id, no RLS.
type ReconciliationRun struct {
	ID    int64     `gorm:"primaryKey;autoIncrement" json:"id"`
	RunAt time.Time `json:"run_at"`

	WalletBalanceKobo       money.Kobo  `gorm:"column:wallet_balance_kobo;type:bigint" json:"wallet_balance_kobo"`
	TotalCashSettlementKobo money.Kobo  `gorm:"column:total_cash_settlement_kobo;type:bigint" json:"total_cash_settlement_kobo"`
	DriftKobo               *money.Kobo `gorm:"column:drift_kobo;type:bigint" json:"drift_kobo,omitempty"`
	Alerted                 bool        `json:"alerted"`

	CreatedAt time.Time `json:"created_at"`
}

func (ReconciliationRun) TableName() string { return "reconciliation_runs" }
