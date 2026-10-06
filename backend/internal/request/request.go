// Package request owns BuyerRequest: the structured representation of what
// a buyer is looking for. Per PRD 1 §5-6/§48: this is deliberately separate
// from the raw WhatsApp message and from the AI's extraction — "AI
// extracted something != Middleman confirmed the request."
package request

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Status is the request lifecycle. CORRECTED is intentionally not a
// status (per PRD 1 §6) — corrections are tracked via audit/events while
// the status remains NEEDS_REVIEW until the operator confirms.
type Status string

const (
	StatusExtracting  Status = "EXTRACTING"
	StatusNeedsReview Status = "NEEDS_REVIEW"
	StatusConfirmed   Status = "CONFIRMED"
)

// ErrNotConfirmed is returned whenever an unconfirmed BuyerRequest would
// otherwise progress into marketplace execution (matching, vendor routing,
// offer generation/delivery, selection, transaction). Phase 3.1 §3A-2.
var ErrNotConfirmed = errors.New("buyer request is not confirmed")

// ErrInvalidTransition is returned when a status change is not allowed
// from the request's current status (e.g. confirming while EXTRACTING).
var ErrInvalidTransition = errors.New("invalid buyer request status transition")

// EnsureConfirmed enforces the invariant
// "unconfirmed BuyerRequest -> cannot progress into marketplace execution".
// Every execution entry point calls this rather than trusting its caller.
func (r *BuyerRequest) EnsureConfirmed() error {
	if r == nil || r.Status != StatusConfirmed {
		return ErrNotConfirmed
	}
	return nil
}

// FunnelDims returns the request's funnel dimensions (event payload keys,
// see internal/event). Only known values are included: category, product
// and location appear once extraction/correction has produced them; they
// are never inferred here.
func (r *BuyerRequest) FunnelDims() map[string]any {
	if r == nil {
		return map[string]any{}
	}
	d := map[string]any{
		"buyer_request_id": r.ID.String(),
		"buyer_id":         r.BuyerID.String(),
		"conversation_id":  r.ConversationID.String(),
		"request_status":   string(r.Status),
	}
	if r.Origin != "" {
		d["origin"] = string(r.Origin)
	}
	if r.ListingID != nil {
		d["supply_listing_id"] = r.ListingID.String()
	}
	if r.DiscoveryDeliveryID != nil {
		d["discovery_delivery_id"] = r.DiscoveryDeliveryID.String()
	}
	if r.Category != nil && *r.Category != "" {
		d["category"] = *r.Category
	}
	if r.Product != nil && *r.Product != "" {
		d["product"] = *r.Product
	}
	if r.Location != nil && *r.Location != "" {
		d["location"] = *r.Location
	}
	if r.BudgetMax != nil {
		d["budget_max"] = *r.BudgetMax
	}
	return d
}

// Completeness is an operator-assistance signal, not a gate (PRD 1 §29-30):
// the system must never reject a request merely because optional fields
// are missing.
type Completeness string

const (
	CompletenessComplete     Completeness = "complete"
	CompletenessPartial      Completeness = "partially_complete"
	CompletenessInsufficient Completeness = "insufficient"
)

// Origin is how demand entered Majo (Architecture Lock V2 §E). One
// BuyerRequest model serves every origin; origin and listing link are
// attributes of the same demand object, not separate objects.
type Origin string

const (
	OriginMessage     Origin = "message"      // WhatsApp free text (extraction)
	OriginDiscovery   Origin = "discovery"    // "Ask about it" on a discovery card
	OriginListingLink Origin = "listing_link" // a shared L- reference (reserved)
)

// ErrDuplicate is returned by Repository.Create when the same listing-origin
// demand already exists (e.g. the same "Ask about it" tap delivered twice).
var ErrDuplicate = errors.New("buyer request already exists for this origin")

// BuyerRequest represents the buyer's current procurement need.
type BuyerRequest struct {
	ID              uuid.UUID
	ConversationID  uuid.UUID
	BuyerID         uuid.UUID
	OriginMessageID *uuid.UUID

	// Phase 4B demand bridge. Origin defaults to OriginMessage. ListingID
	// and DiscoveryDeliveryID attribute listing-origin demand to the
	// SupplyListing and the DiscoveryDelivery it came from.
	Origin              Origin
	ListingID           *uuid.UUID
	DiscoveryDeliveryID *uuid.UUID

	Status Status

	Category *string
	Product  *string
	Variant  *string

	BudgetMin *int64
	BudgetMax *int64
	Currency  string

	Location    *string
	Condition   *string
	Quantity    *int
	Urgency     *string
	Preferences []string

	RawMessage    string
	ExtractedData map[string]any // original AI output — never overwritten by corrections
	Confidence    float64

	Completeness Completeness

	CreatedAt   time.Time
	UpdatedAt   time.Time
	ConfirmedAt *time.Time
}

// CalculateCompleteness applies the lightweight rule from PRD 1 §30:
// product identified + budget known + location known are the signals
// used to assist the operator, not to gate anything.
func (r *BuyerRequest) CalculateCompleteness() Completeness {
	hasProduct := r.Product != nil && *r.Product != ""
	hasBudget := r.BudgetMin != nil || r.BudgetMax != nil
	hasLocation := r.Location != nil && *r.Location != ""

	switch {
	case hasProduct && hasBudget && hasLocation:
		return CompletenessComplete
	case hasProduct && (hasBudget || hasLocation):
		return CompletenessPartial
	case hasProduct:
		return CompletenessPartial
	default:
		return CompletenessInsufficient
	}
}

// Fields is the set of correctable structured fields, used by the
// correction/audit path to build previous/new value diffs generically.
type Fields struct {
	Category    *string
	Product     *string
	Variant     *string
	BudgetMin   *int64
	BudgetMax   *int64
	Currency    *string
	Location    *string
	Condition   *string
	Quantity    *int
	Urgency     *string
	Preferences []string
}

// Repository is the persistence boundary for BuyerRequest.
type Repository interface {
	Create(ctx context.Context, r *BuyerRequest) (*BuyerRequest, error)
	FindByID(ctx context.Context, id uuid.UUID) (*BuyerRequest, error)
	List(ctx context.Context, statusFilter *Status) ([]*BuyerRequest, error)
	UpdateExtraction(ctx context.Context, id uuid.UUID, fields Fields, extractedData map[string]any, confidence float64, newStatus Status) (*BuyerRequest, error)
	UpdateFields(ctx context.Context, id uuid.UUID, fields Fields) (*BuyerRequest, error)
	Confirm(ctx context.Context, id uuid.UUID) (*BuyerRequest, error)
	// FindDiscoveryAsk returns the discovery-origin request for this
	// (delivery, listing), or nil (idempotent "Ask about it").
	FindDiscoveryAsk(ctx context.Context, deliveryID, listingID uuid.UUID) (*BuyerRequest, error)
	// FindLatestOpenByConversation supports the clarification flow (PRD 1 §28):
	// a follow-up buyer message should continue the same request rather than
	// always starting a new one, if one is still open (not yet confirmed).
	FindLatestOpenByConversation(ctx context.Context, conversationID uuid.UUID) (*BuyerRequest, error)
}
