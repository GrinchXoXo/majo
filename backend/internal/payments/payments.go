// Package payments is the payment-provider boundary (pilot batch item 8,
// monetization scaffold). It replaces the Phase 0 placeholder in
// infrastructure/payment.
//
// Only the "none" provider exists. Choosing any other PAYMENTS_PROVIDER stops
// the server at startup until that provider is built, reviewed, and its
// legal and business gate is confirmed by the owner
// (docs/architecture/monetization-and-payments-rollout.md).
//
// The provider kinds follow the rollout stages:
//
//	none            Stage A: Majo takes no money.
//	payment_link    Stage B: checkout links for Majo's own income (lead
//	                credits, Vendor Pro, verified badge, rush, sponsored).
//	split           Stage B, optional: the provider splits the buyer's payment
//	                straight to the vendor's sub-account minus Majo's fee.
//	                Majo never holds the vendor's share (not escrow).
//	escrow_partner  Stage C: a licensed partner holds the buyer's deposit and
//	                releases it on Majo's instruction.
package payments

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Kind of provider.
type Kind string

const (
	KindNone          Kind = "none"
	KindPaymentLink   Kind = "payment_link"
	KindSplit         Kind = "split"
	KindEscrowPartner Kind = "escrow_partner"
)

// ParseKind reads PAYMENTS_PROVIDER ("" = none).
func ParseKind(s string) (Kind, error) {
	switch k := Kind(strings.ToLower(strings.TrimSpace(s))); k {
	case "":
		return KindNone, nil
	case KindNone, KindPaymentLink, KindSplit, KindEscrowPartner:
		return k, nil
	}
	return KindNone, fmt.Errorf("PAYMENTS_PROVIDER must be none, payment_link, split or escrow_partner, got %q", s)
}

// ErrDisabled: no payment provider is configured (Stage A).
var ErrDisabled = errors.New("payments are disabled (PAYMENTS_PROVIDER=none)")

// CheckoutRequest asks for a payment link. Amounts are integer kobo.
type CheckoutRequest struct {
	Reference   string // Majo's idempotency reference, unique per payment
	Line        string // monetization line, e.g. LEAD_CREDITS
	AmountKobo  int64
	Currency    string // NGN
	Description string
	PayerPhone  string
	PayerEmail  string // most Nigerian providers require one
	CallbackURL string
	Metadata    map[string]string
}

// Validate the request before any provider is called.
func (r CheckoutRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.Reference) == "":
		return errors.New("payments: reference is required")
	case r.AmountKobo <= 0:
		return errors.New("payments: amount must be positive")
	case len(r.Currency) != 3:
		return errors.New("payments: currency must be a 3-letter code")
	}
	return nil
}

// Checkout is a created payment link.
type Checkout struct {
	ProviderRef string
	URL         string
	ExpiresAt   *time.Time
}

// EventType of a provider notification.
type EventType string

const (
	ChargeSucceeded EventType = "charge.succeeded"
	ChargeFailed    EventType = "charge.failed"
	ChargeRefunded  EventType = "charge.refunded"
)

// Event is a verified provider notification (webhook or status check).
type Event struct {
	Type        EventType
	Reference   string // Majo's reference from the CheckoutRequest
	ProviderRef string
	AmountKobo  int64
	FeeKobo     int64 // the provider's fee, when reported
	Currency    string
	At          time.Time
}

// Provider is implemented by every payment provider.
type Provider interface {
	Kind() Kind
	CreateCheckout(ctx context.Context, req CheckoutRequest) (*Checkout, error)
	// VerifyCharge asks the provider for the final state of a payment;
	// never trust a redirect or a client-side callback alone.
	VerifyCharge(ctx context.Context, reference string) (*Event, error)
	// ParseWebhook verifies the provider's signature, then parses. An
	// unsigned or wrongly signed body is an error.
	ParseWebhook(header http.Header, body []byte) (*Event, error)
	Refund(ctx context.Context, providerRef string, amountKobo int64) error
}

// SplitInstruction: the vendor's share settles directly to their
// sub-account; Majo receives only its fee.
type SplitInstruction struct {
	VendorSubaccount string
	MajoFeeKobo      int64
	FeeBearer        string // who pays the provider fee: "vendor" or "majo" (open decision)
}

// SplitProvider is a payment_link provider that can split.
type SplitProvider interface {
	Provider
	CreateSplitCheckout(ctx context.Context, req CheckoutRequest, split SplitInstruction) (*Checkout, error)
}

// HoldRequest: the buyer's deposit for a deal, held by the partner.
type HoldRequest struct {
	Reference  string
	DealID     string
	AmountKobo int64
	Currency   string
	ExpiresAt  time.Time // refund on expiry is one of the five open decisions
}

// Hold is a deposit held by the partner.
type Hold struct {
	ProviderRef string
	Status      string
}

// EscrowPartner is a licensed partner that holds funds (Stage C).
type EscrowPartner interface {
	Provider
	Hold(ctx context.Context, req HoldRequest) (*Hold, error)
	Release(ctx context.Context, holdRef string, toVendorKobo, majoFeeKobo int64) error
	Return(ctx context.Context, holdRef string) error
}

// None is the Stage A provider: every call fails with ErrDisabled.
type None struct{}

func (None) Kind() Kind { return KindNone }
func (None) CreateCheckout(context.Context, CheckoutRequest) (*Checkout, error) {
	return nil, ErrDisabled
}
func (None) VerifyCharge(context.Context, string) (*Event, error) { return nil, ErrDisabled }
func (None) ParseWebhook(http.Header, []byte) (*Event, error)     { return nil, ErrDisabled }
func (None) Refund(context.Context, string, int64) error          { return ErrDisabled }

// New returns the configured provider. Only "none" is built; any other kind
// is refused so a configuration change alone can never start taking money.
func New(kind Kind) (Provider, error) {
	if kind == KindNone || kind == "" {
		return None{}, nil
	}
	return nil, fmt.Errorf("PAYMENTS_PROVIDER=%s is not built yet: it needs its own reviewed change and the owner's go-ahead on the legal and business gate (docs/architecture/monetization-and-payments-rollout.md)", kind)
}
