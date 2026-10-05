// Package payment defines the PaymentProvider boundary.
// Per PRD 0 FINAL §18 and Middleman_Technical_Spec_v1 §7: this interface
// must exist now so business logic never couples to a specific provider,
// but no implementation (Paystack/Flutterwave/Kredi) is built in Phase 0.
// Payments are explicitly out of scope until the transaction engine phase.
package payment

import "context"

type Status string

const (
	StatusPending  Status = "PENDING"
	StatusSuccess  Status = "SUCCESS"
	StatusFailed   Status = "FAILED"
	StatusRefunded Status = "REFUNDED"
)

type CreatePaymentRequest struct {
	TransactionID string // references transaction.Transaction.ID (Phase 3+)
	AmountKobo    int64  // Naira amounts stored/transmitted in kobo to avoid float rounding
	BuyerRef      string
}

type PaymentResult struct {
	PaymentID string
	Status    Status
	RedirectURL string // for providers that require a hosted checkout step
}

// Provider is intentionally unimplemented — it exists so that when
// payments are introduced (Paystack/Flutterwave/Kredi/etc.), no other
// code needs to change. Phase 3 introduces the Transaction domain that
// this interface will eventually be wired to; it does not implement or
// call this interface yet (PRD 3 §22: "do not implement payment
// processing... create a clean provider boundary").
type Provider interface {
	CreatePayment(ctx context.Context, req CreatePaymentRequest) (*PaymentResult, error)
	GetPayment(ctx context.Context, paymentID string) (*PaymentResult, error)
	VerifyPayment(ctx context.Context, paymentID string) (*PaymentResult, error)
	RefundPayment(ctx context.Context, paymentID string) error
}
