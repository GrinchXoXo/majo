// Package offer owns Offer: the buyer-facing representation of something
// that can fulfil a BuyerRequest. Per PRD 3 §15-18: an Offer may
// originate from a VendorResponse (request-specific) and/or point at a
// pre-existing SupplyListing (discovery-specific) — both origins are
// first-class, not one primary path with a bolted-on exception.
package offer

import (
	"context"
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
)

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
}

// Fields supports partial updates (status transitions, operator edits).
type Fields struct {
	Status *Status
	Notes  *string
}

// Repository is the persistence boundary for Offer.
type Repository interface {
	Create(ctx context.Context, o *Offer) (*Offer, error)
	Update(ctx context.Context, id uuid.UUID, fields Fields) (*Offer, error)
	FindByID(ctx context.Context, id uuid.UUID) (*Offer, error)
	ListByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*Offer, error)
	// FindByVendorResponseID supports eligibility checks: has this
	// response already produced an offer? (avoids duplicate offers if
	// re-run).
	FindByVendorResponseID(ctx context.Context, vendorResponseID uuid.UUID) (*Offer, error)
}
