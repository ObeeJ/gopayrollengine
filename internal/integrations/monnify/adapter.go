package monnify

import (
	"context"
	"fmt"
	"os"

	"go-payroll-engine/internal/integrations/provider"
	"go-payroll-engine/pkg/money"
)

// Adapter satisfies provider.Provider using the existing Monnify Client.
// Monnify's disbursement API is bulk-only — there is no documented
// single-transfer endpoint this codebase has verified — so a single payout is
// submitted as a one-item batch. That is not a workaround bolted on for this
// adapter: it is exactly how the payroll worker already disburses each
// individual employee payment, so a single-item EWA advance and a
// single-employee payroll batch go through the identical wire call.
type Adapter struct {
	client *Client
}

// NewAdapter wraps an existing Monnify Client as a provider.Provider.
func NewAdapter(client *Client) *Adapter {
	return &Adapter{client: client}
}

func (a *Adapter) Name() string { return "monnify" }

// SupportsCurrency — Monnify's disbursement API is NGN-only. Verified against
// its own request examples, not the live API reference (egress-blocked from
// this environment) — re-confirm before depending on this for a market
// decision.
func (a *Adapter) SupportsCurrency(c money.Currency) bool {
	return c == money.NGN
}

func (a *Adapter) InitiateTransfer(_ context.Context, req provider.TransferRequest) (provider.TransferResult, error) {
	if !a.SupportsCurrency(req.Amount.Currency) {
		return provider.TransferResult{}, fmt.Errorf("%w: %s", provider.ErrCurrencyUnsupported, req.Amount.Currency)
	}

	naira, err := req.Amount.Kobo()
	if err != nil {
		// Unreachable given the SupportsCurrency guard above, but a wrong
		// answer here is a wrong disbursement amount, so fail rather than guess.
		return provider.TransferResult{}, fmt.Errorf("monnify adapter: %w", err)
	}

	resp, err := a.client.InitiateBulkTransfer(BulkTransferRequest{
		Title:                     "Disbursement " + req.Reference,
		BatchReference:            req.Reference,
		SourceWalletAccountNumber: os.Getenv("MONNIFY_SOURCE_WALLET"),
		TransactionList: []TransferDetail{{
			Amount:        naira.Naira(),
			AccountNumber: req.RecipientAccountNumber,
			BankCode:      req.RecipientBankCode,
			Narration:     req.Narration,
			Reference:     req.Reference,
			CurrencyCode:  string(money.NGN),
		}},
	})
	if err != nil {
		return provider.TransferResult{}, fmt.Errorf("%w: %w", provider.ErrProviderUnavailable, err)
	}

	return provider.TransferResult{
		Accepted:          resp.RequestSuccessful,
		ProviderReference: resp.ResponseBody.BatchReference,
		Message:           resp.ResponseMessage,
	}, nil
}

// GetTransferStatus — Monnify's per-item status lives in the webhook payload,
// not a documented polling endpoint this codebase has verified against. Until
// that is confirmed, reconciliation for Monnify-routed transfers relies on the
// webhook arriving rather than active polling; returning StatusUnknown here is
// honest about that gap rather than guessing at an endpoint shape.
func (a *Adapter) GetTransferStatus(context.Context, string) (provider.TransferStatus, error) {
	return provider.StatusUnknown, nil
}
