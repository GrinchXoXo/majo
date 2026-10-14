// Package supply owns SupplySignal: what a vendor has indicated they
// currently have, can source, or can otherwise supply. Per PRD 2 §5.2/§12:
// signals are time-sensitive and never treated as a permanent catalog.
package supply

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/event"
)

type Availability string

const (
	AvailabilityAvailable  Availability = "AVAILABLE"
	AvailabilityCanSource  Availability = "CAN_SOURCE"
	AvailabilityUnavailable Availability = "UNAVAILABLE"
	AvailabilityExpired    Availability = "EXPIRED"
)

// Signal is a single vendor-submitted supply claim.
type Signal struct {
	ID                 uuid.UUID
	VendorID           uuid.UUID
	Category           *string
	Product            *string
	NormalizedProduct  *string
	Variant            *string
	Condition          *string
	Price              *int64
	Currency           string
	Quantity           *int
	Location           *string
	AvailabilityStatus Availability
	RawMessage         string
	ExtractedData      map[string]any
	Confidence         float64
	CreatedAt          time.Time
	UpdatedAt          time.Time
	ExpiresAt          time.Time
}

// Repository is the persistence boundary for supply signals.
type Repository interface {
	Create(ctx context.Context, s *Signal) (*Signal, error)
	FindByID(ctx context.Context, id uuid.UUID) (*Signal, error)
	ListByVendor(ctx context.Context, vendorID uuid.UUID) ([]*Signal, error)
	// FreshByNormalizedProduct returns non-expired signals matching a
	// normalized product name, across all vendors — the matching engine's
	// primary supply-side input (PRD 2 §42: "supply freshness ... improve
	// vendor relevance").
	FreshByNormalizedProduct(ctx context.Context, normalizedProduct string) ([]*Signal, error)
	FreshByVendor(ctx context.Context, vendorID uuid.UUID) ([]*Signal, error)
	Update(ctx context.Context, id uuid.UUID, fields SignalFields) (*Signal, error)
}

// SignalFields supports the operator-correction path (PRD 2 §38-like, but
// for supply rather than vendor responses).
type SignalFields struct {
	Category  *string
	Product   *string
	Variant   *string
	Condition *string
	Price     *int64
	Quantity  *int
	Location  *string
	Availability *Availability
}

// --- Phase 3: SupplyListing ---
//
// Listing is durable marketplace supply — "Majo currently considers
// this available for discovery/matching" — distinct from Signal, which
// only records "the vendor told us this" (PRD 3 §5). A Listing always
// traces back to the Signal that produced it (SignalID), preserving
// provenance for freshness/duplicate-detection/audit purposes.
type ListingStatus string

const (
	ListingDraft       ListingStatus = "DRAFT"
	ListingActive      ListingStatus = "ACTIVE"
	ListingPaused      ListingStatus = "PAUSED"
	ListingSold        ListingStatus = "SOLD"
	ListingUnavailable ListingStatus = "UNAVAILABLE"
	ListingExpired     ListingStatus = "EXPIRED"
	ListingArchived    ListingStatus = "ARCHIVED"
)

// MediaReference is a provider-agnostic pointer to vendor media (an
// image today, potentially audio/video later) — never tightly coupled to
// a single WhatsApp-specific media implementation (PRD 3 §9).
type MediaReference struct {
	Type       string `json:"type"` // "image" | "audio" | "video" | "document"
	StorageKey string `json:"storage_key,omitempty"`
	URL        string `json:"url,omitempty"`
	// AssetID is the MediaAsset this reference points at (Phase 4A.3).
	// The asset stays the single stored copy; the listing only refers to it.
	AssetID string `json:"asset_id,omitempty"`
	// Role is MediaRolePrimary on the operator-chosen primary image (Phase
	// 4A Step 4), empty otherwise. With no PRIMARY reference the first
	// image is the primary (Architecture Lock V2). Order = array order.
	Role string `json:"role,omitempty"`
}

// MediaRolePrimary marks the listing's primary image.
const MediaRolePrimary = "PRIMARY"

// PrimaryMedia returns the effective primary image: the reference marked
// PRIMARY, else the first image, else nil.
func PrimaryMedia(refs []MediaReference) *MediaReference {
	for i := range refs {
		if refs[i].Role == MediaRolePrimary {
			return &refs[i]
		}
	}
	for i := range refs {
		if refs[i].Type == "image" {
			return &refs[i]
		}
	}
	return nil
}

type Listing struct {
	ID                uuid.UUID
	VendorID          uuid.UUID
	SupplySignalID    *uuid.UUID
	// PublicReference is the human-facing "L-XXXXXX" identity (Phase 4A.3).
	PublicReference string

	Category          *string
	Product           *string
	NormalizedProduct *string
	Variant           *string
	Condition         *string
	Price             *int64
	Currency          string
	Quantity          *int
	Location          *string

	Description      *string
	MediaReferences  []MediaReference

	Status ListingStatus

	SourceChannel    string
	SourceMessageID  *uuid.UUID
	ExtractionMethod string
	Confidence       float64

	PublishedAt    *time.Time
	LastVerifiedAt *time.Time
	ExpiresAt      time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time

	// RejectedAt/RejectionReason (Phase 4A Step 4) are set when an operator
	// rejected the listing; a rejected listing is ARCHIVED.
	RejectedAt      *time.Time
	RejectionReason *string
}

// ListingFields supports partial updates of marketplace fields. Nil means
// "leave unchanged". There is deliberately no Status here (Phase 4A Step
// 4): state transitions only happen through the dedicated curation
// operations (ListingService.Publish / Reject), never a field update.
type ListingFields struct {
	Category        *string
	Product         *string
	Variant         *string
	Condition       *string
	Price           *int64
	Currency        *string
	Quantity        *int
	Location        *string
	Description     *string
	MediaReferences *[]MediaReference
}

// ListingQuery supports the discovery/search foundation from PRD 3 §14.
// All fields are optional filters; a nil field means "don't filter on
// this". Kept flat and simple — no query DSL.
type ListingQuery struct {
	Category         *string
	NormalizedProduct *string
	PriceMin         *int64
	PriceMax         *int64
	Location         *string
	Condition        *string
	Status           *ListingStatus
	VendorID         *uuid.UUID
	// Lifecycle (Phase 4A.5): "" or "live" = inside the lifecycle ceiling
	// (default), "expired" = past it (history), "all" = both.
	Lifecycle string
}

// ListingRepository is the persistence boundary for SupplyListing.
// Deliberately a separate interface from Repository (Signal) — they are
// different lifecycles/tables (PRD 3 §5: "do not collapse these
// concepts").
type ListingRepository interface {
	Create(ctx context.Context, l *Listing) (*Listing, error)
	Update(ctx context.Context, id uuid.UUID, fields ListingFields) (*Listing, error)
	FindByID(ctx context.Context, id uuid.UUID) (*Listing, error)
	// FindBySignalID supports duplicate detection (PRD 3 §11): has this
	// signal already produced a listing?
	FindBySignalID(ctx context.Context, signalID uuid.UUID) (*Listing, error)
	// FindRecentDuplicate looks for an existing non-expired listing from
	// the same vendor with the same normalized product+variant+price —
	// simple, deterministic duplicate detection, no ML (PRD 3 §11).
	FindRecentDuplicate(ctx context.Context, vendorID uuid.UUID, normalizedProduct string, variant *string, price *int64) (*Listing, error)
	Search(ctx context.Context, q ListingQuery) ([]*Listing, error)
	ListByVendor(ctx context.Context, vendorID uuid.UUID) ([]*Listing, error)
	// CreateFromSignal (Phase 4A.3) persists a listing built from a signal,
	// atomically and race-safely (one transaction, serialised per vendor):
	//   - a live listing already exists for this signal -> returns it
	//     (ListingExisting), first appending any of w.Listing's media
	//     references it lacks if it is still a DRAFT (ListingMediaSynced);
	//   - a Phase 3 duplicate exists (same vendor + normalized product +
	//     variant + price, not expired/archived) -> only its freshness is
	//     advanced (ListingRefreshed); other fields are never overwritten;
	//   - otherwise inserts w.Listing with a fresh unique public reference
	//     (ListingCreated).
	// The matching event callback is written in the same transaction.
	CreateFromSignal(ctx context.Context, w ListingWrite) (*Listing, ListingOutcome, error)
	// Curate (Phase 4A Step 4) is the only write path for operator
	// curation. In ONE transaction it locks the listing row, passes the
	// current row to decide, and applies the returned change (nil = no-op)
	// together with its audit entry and event. Returns ErrListingNotFound
	// for an unknown listing, and decide's error unchanged.
	Curate(ctx context.Context, id uuid.UUID, decide CurationDecision) (*Listing, error)
}

// ListingOutcome says what CreateFromSignal did.
type ListingOutcome string

const (
	ListingCreated     ListingOutcome = "CREATED"
	ListingExisting    ListingOutcome = "EXISTING"
	ListingMediaSynced ListingOutcome = "MEDIA_SYNCED"
	ListingRefreshed   ListingOutcome = "REFRESHED"
)

// ListingWrite is the input of CreateFromSignal. The callbacks build the
// event for whichever outcome happens (nil callback or nil event = none).
type ListingWrite struct {
	Listing     *Listing
	OnCreate    func(created *Listing) *event.Event
	OnRefresh   func(existing *Listing) *event.Event
	OnMediaSync func(existing *Listing, added []MediaReference) *event.Event
}
