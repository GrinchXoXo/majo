// Package offer owns Offer: the buyer-facing representation of something
// that can fulfil a BuyerRequest. Per PRD 3 §15-18: an Offer may
// originate from a VendorResponse (request-specific) and/or point at a
// pre-existing SupplyListing (discovery-specific) — both origins are
// first-class, not one primary path with a bolted-on exception.
package offer

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

type Type string

const (
	TypeExact       Type = "EXACT"
	TypeAlternative Type = "ALTERNATIVE"
	TypeCanSource   Type = "CAN_SOURCE"
	TypePartial     Type = "PARTIAL"
)

type Status string

const (
	StatusDraft       Status = "DRAFT"
	StatusReviewed    Status = "REVIEWED"
	StatusShortlisted Status = "SHORTLISTED"
	StatusSent        Status = "SENT"
	StatusAccepted    Status = "ACCEPTED"
	StatusRejected    Status = "REJECTED"
	StatusExpired     Status = "EXPIRED"

	// --- Phase 3.1 ---
	// StatusSending: claimed for delivery by exactly one deliverer; the
	// WhatsApp send is in flight. Returns to REVIEWED if the send fails.
	StatusSending Status = "SENDING"
	// StatusWithdrawn: the source VendorResponse stopped being eligible
	// (e.g. corrected to UNAVAILABLE) before the buyer chose this offer.
	StatusWithdrawn Status = "WITHDRAWN"
	// StatusDuplicate: a pre-3.1 duplicate of another offer for the same
	// VendorResponse, retired by migration 0006. Never shown or selectable.
	StatusDuplicate Status = "DUPLICATE"
)

// ErrAlreadyExists is returned by Repository.Create when an offer for the
// same VendorResponse already exists (DB unique index — one logical Offer
// per VendorResponse, Phase 3.1).
var ErrAlreadyExists = errors.New("offer for this vendor response already exists")

// SelectionWindow is how long after delivery a buyer can still select an
// offer by replying with its number. Vendor prices/availability move fast;
// beyond this the buyer is asked to start again rather than committing to
// stale information. Deliberately a single named constant (not config) —
// a product decision to revisit with real data.
const SelectionWindow = 48 * time.Hour

// Offer is a ranked, buyer-facing candidate for fulfilling a
// BuyerRequest. VendorResponseID and SupplyListingID are both nullable —
// exactly the PRD 3 §17 requirement that neither origin is a second-class
// citizen — but at least one must be set (enforced at the DB level).
type Offer struct {
	ID               uuid.UUID
	BuyerRequestID   uuid.UUID
	VendorRequestID  *uuid.UUID
	VendorResponseID *uuid.UUID
	SupplyListingID  *uuid.UUID
	VendorID         uuid.UUID

	Type   Type
	Status Status

	Product   *string
	Variant   *string
	Condition *string
	Price     *int64
	Currency  string
	Quantity  *int
	Location  *string
	Notes     *string

	RankingScore   int
	RankingReasons []string
	Confidence     float64

	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt *time.Time

	// --- Phase 3.1 delivery tracking ---
	// PresentationOrdinal is the number the buyer replies with ("1").
	// Assigned once at first delivery; stable for the life of the request.
	PresentationOrdinal  *int
	SentAt               *time.Time
	DeliveryAttempts     int
	LastDeliveryError    *string
	LastDeliveryFailedAt *time.Time
}

// IsSelectable reports whether a buyer (or an operator acting for one) may
// still select this offer at time now. Delivered offers expire after
// SelectionWindow; an undelivered REVIEWED offer remains selectable through
// the operator override path.
func (o *Offer) IsSelectable(now time.Time) bool {
	switch o.Status {
	case StatusSent, StatusReviewed:
	default:
		return false
	}
	if o.ExpiresAt != nil && !now.Before(*o.ExpiresAt) {
		return false
	}
	return true
}

// Fields supports partial updates (status transitions, operator edits).
type Fields struct {
	Status *Status
	Notes  *string
}

// Repository is the persistence boundary for Offer.
type Repository interface {
	// Create persists a new offer. Returns ErrAlreadyExists if an offer for
	// the same VendorResponse already exists.
	Create(ctx context.Context, o *Offer) (*Offer, error)
	Update(ctx context.Context, id uuid.UUID, fields Fields) (*Offer, error)
	FindByID(ctx context.Context, id uuid.UUID) (*Offer, error)
	ListByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*Offer, error)
	// FindByVendorResponseID supports eligibility checks: has this
	// response already produced an offer? (avoids duplicate offers if
	// re-run).
	FindByVendorResponseID(ctx context.Context, vendorResponseID uuid.UUID) (*Offer, error)
}

// DeliveryRepository is the persistence boundary for buyer-facing offer
// delivery (Phase 3.1). Kept separate from Repository so read/build code
// paths don't depend on delivery mechanics.
type DeliveryRepository interface {
	// ClaimForDelivery atomically claims every not-yet-delivered offer for a
	// buyer request (status REVIEWED, or SENDING left over from a crashed
	// attempt older than staleAfter), moves them to SENDING and assigns each
	// a stable presentation ordinal continuing after the highest ordinal
	// already used for that request. Concurrent callers for the same request
	// are serialized; a second caller receives only what the first did not
	// claim (usually nothing). Returns the claimed offers in ordinal order
	// and the highest ordinal now in use for the request.
	ClaimForDelivery(ctx context.Context, buyerRequestID uuid.UUID, staleAfter time.Duration) (claimed []*Offer, highestOrdinal int, err error)
	// MarkDelivered moves claimed offers SENDING -> SENT and stamps sent_at
	// and expires_at.
	MarkDelivered(ctx context.Context, offerIDs []uuid.UUID, sentAt, expiresAt time.Time) error
	// MarkDeliveryFailed returns claimed offers SENDING -> REVIEWED, releases
	// their ordinals, and records the error, so a later attempt can retry.
	MarkDeliveryFailed(ctx context.Context, offerIDs []uuid.UUID, deliveryErr string) error
	// FindLatestPresentedRequest returns the buyer request (within the
	// conversation) whose offers were most recently delivered to the buyer,
	// or nil if none. This is the buyer's current offer-selection context.
	FindLatestPresentedRequest(ctx context.Context, conversationID uuid.UUID) (*uuid.UUID, error)
}
