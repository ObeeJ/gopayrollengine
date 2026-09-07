package services

import (
	"context"
	"errors"
	"os"

	"go-payroll-engine/internal/integrations/monnify"
	"go-payroll-engine/internal/models"
	"go-payroll-engine/pkg/money"

	"gorm.io/gorm"
)

// ErrFundingAccountProviderRejected is returned when the provider accepts the
// request but reports it did not succeed — distinct from a network/transport
// error so the handler can tell the two apart.
var ErrFundingAccountProviderRejected = errors.New("funding: provider rejected reserved account request")

// FundingService provisions and reports on each org's dedicated deposit
// account — see migration 000019 and models.RecordEmployerFunding for why
// this exists and how a deposit reaches the ledger.
type FundingService struct {
	monnify *monnify.Client
}

// NewFundingService wires up the service against a Monnify client.
func NewFundingService(client *monnify.Client) *FundingService {
	return &FundingService{monnify: client}
}

// ProvisionAccount returns the org's dedicated funding account, creating it
// on first use. Idempotent: a second call for an org that already has one
// returns the existing row without calling the provider again — Monnify
// itself is also idempotent on AccountReference, so a retry after a crash
// between the provider call and the local write still cannot mint a second
// account, only re-fetch the same one.
//
// contactEmail is required from the caller rather than synthesized: an
// Organization has no email on file today (auth is org ID + password), and
// fabricating a placeholder would silently hand the provider an address
// nobody reads deposit notifications from.
func (s *FundingService) ProvisionAccount(ctx context.Context, orgID, contactEmail string) (*models.OrganizationFundingAccount, error) {
	var account *models.OrganizationFundingAccount
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		var existing models.OrganizationFundingAccount
		err := tx.First(&existing, "organization_id = ?", orgID).Error
		if err == nil {
			account = &existing
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		var org models.Organization
		if err := tx.First(&org, "id = ?", orgID).Error; err != nil {
			return err
		}

		ref := "FUND-" + orgID
		resp, err := s.monnify.CreateReservedAccount(monnify.ReserveAccountRequest{
			AccountReference: ref,
			AccountName:      org.Name,
			CurrencyCode:     string(money.NGN),
			ContractCode:     os.Getenv("MONNIFY_CONTRACT_CODE"),
			CustomerName:     org.Name,
			CustomerEmail:    contactEmail,
		})
		if err != nil {
			return err
		}
		if !resp.RequestSuccessful || len(resp.ResponseBody.Accounts) == 0 {
			return ErrFundingAccountProviderRejected
		}
		acc := resp.ResponseBody.Accounts[0]

		record := models.OrganizationFundingAccount{
			OrganizationID:   orgID,
			ProviderName:     "monnify",
			AccountReference: resp.ResponseBody.AccountReference,
			AccountNumber:    acc.AccountNumber,
			AccountName:      resp.ResponseBody.AccountName,
			BankName:         acc.BankName,
			BankCode:         acc.BankCode,
		}
		if err := tx.Create(&record).Error; err != nil {
			return err
		}
		account = &record
		return models.AppendAuditTx(tx, orgID, "OrganizationFundingAccount", orgID, "provisioned",
			"", record.AccountNumber, "", "")
	})
	return account, err
}

// GetFundingStatus reports the org's account (if provisioned) and its current
// exposure — see models.FundingExposure.
type FundingStatus struct {
	Account  *models.OrganizationFundingAccount `json:"account,omitempty"`
	Exposure money.Money                        `json:"exposure"`
}

func (s *FundingService) GetFundingStatus(ctx context.Context, orgID string) (*FundingStatus, error) {
	status := &FundingStatus{}
	err := models.WithOrgScope(ctx, orgID, func(tx *gorm.DB) error {
		var account models.OrganizationFundingAccount
		if err := tx.First(&account, "organization_id = ?", orgID).Error; err == nil {
			status.Account = &account
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		exposure, err := models.FundingExposure(tx, orgID, money.NGN)
		if err != nil {
			return err
		}
		status.Exposure = exposure
		return nil
	})
	return status, err
}
