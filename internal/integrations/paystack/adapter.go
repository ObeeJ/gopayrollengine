package paystack

import (
	"context"
	"fmt"

	"go-payroll-engine/internal/integrations/provider"
	"go-payroll-engine/pkg/money"
)

// Adapter satisfies provider.Provider using the Paystack Client. Paystack's
// transfer flow is two calls (create a recipient, then pay it) where the
// provider.Provider interface exposes one — InitiateTransfer composes both so
// callers never need to know Paystack requires a recipient step at all.
type Adapter struct {
	client *Client
}

func NewAdapter(client *Client) *Adapter {
	return &Adapter{client: client}
}

func (a *Adapter) Name() string { return "paystack" }

// SupportsCurrency — NGN only for now. Paystack settles GHS/ZAR/KES payouts
// too, but each requires a different recipient `type` value (see
// CreateRecipientRequest) that this client has not mapped or verified against
// live docs. Claiming those currencies here without that mapping would create
// exactly the kind of confident-but-wrong currency support this codebase has
// already had to correct once (see the Monnify currency-support fix in the
// commit history). Extend this once the per-country recipient types are
// confirmed, not before.
func (a *Adapter) SupportsCurrency(c money.Currency) bool {
	return c == money.NGN
}

func (a *Adapter) InitiateTransfer(ctx context.Context, req provider.TransferRequest) (provider.TransferResult, error) {
	if !a.SupportsCurrency(req.Amount.Currency) {
		return provider.TransferResult{}, fmt.Errorf("%w: %s", provider.ErrCurrencyUnsupported, req.Amount.Currency)
	}

	recipientCode, err := a.client.CreateRecipient(ctx, CreateRecipientRequest{
		Type:          "nuban",
		Name:          req.RecipientName,
		AccountNumber: req.RecipientAccountNumber,
		BankCode:      req.RecipientBankCode,
		Currency:      string(req.Amount.Currency),
	})
	if err != nil {
		return provider.TransferResult{}, fmt.Errorf("%w: %w", provider.ErrProviderUnavailable, err)
	}

	resp, err := a.client.InitiateTransfer(ctx, InitiateTransferRequest{
		Source:    "balance",
		Amount:    req.Amount.Minor,
		Recipient: recipientCode,
		Reason:    req.Narration,
		Reference: req.Reference,
		Currency:  string(req.Amount.Currency),
	})
	if err != nil {
		return provider.TransferResult{}, fmt.Errorf("%w: %w", provider.ErrProviderUnavailable, err)
	}

	// "otp" means the Paystack account still requires manual OTP confirmation
	// for API-initiated transfers — a dashboard misconfiguration, not a
	// transient failure. Surfaced as a rejection with a message that says so,
	// rather than left for the caller to puzzle out from a bare "not accepted".
	if resp.Data.Status == "otp" {
		return provider.TransferResult{
			Accepted:          false,
			ProviderReference: resp.Data.TransferCode,
			Message:           "paystack account requires OTP confirmation for API transfers; disable OTP for API-initiated transfers in the Paystack dashboard",
		}, nil
	}

	return provider.TransferResult{
		Accepted:          resp.Status && resp.Data.Status != "failed",
		ProviderReference: resp.Data.TransferCode,
		Message:           resp.Message,
	}, nil
}

func (a *Adapter) GetTransferStatus(ctx context.Context, providerReference string) (provider.TransferStatus, error) {
	resp, err := a.client.VerifyTransfer(ctx, providerReference)
	if err != nil {
		return provider.StatusUnknown, fmt.Errorf("%w: %w", provider.ErrProviderUnavailable, err)
	}
	switch resp.Data.Status {
	case "success":
		return provider.StatusSuccessful, nil
	case "failed", "reversed":
		return provider.StatusFailed, nil
	case "pending", "otp":
		return provider.StatusPending, nil
	default:
		return provider.StatusUnknown, nil
	}
}
