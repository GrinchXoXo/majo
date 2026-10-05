// Package routing owns VendorRequest and VendorResponse: the record of
// Middleman asking a specific vendor about a specific BuyerRequest, and
// the vendor's structured reply. Per PRD 2 §31-38 and PRD 2.1 (status vs
// review-state separation, safe association).
package routing

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type VendorRequestStatus string

const (
	VendorRequestPending   VendorRequestStatus = "PENDING"
	VendorRequestSent      VendorRequestStatus = "SENT"
	VendorRequestResponded VendorRequestStatus = "RESPONDED"
	VendorRequestExpired   VendorRequestStatus = "EXPIRED"
	VendorRequestCancelled VendorRequestStatus = "CANCELLED"
)

// VendorRequest represents Middleman asking one vendor about one
// BuyerRequest. PublicReference is what the vendor actually sees
// (PRD 2 §31: never expose internal UUIDs to vendors).
type VendorRequest struct {
	ID              uuid.UUID
	BuyerRequestID  uuid.UUID
	VendorID        uuid.UUID
	PublicReference string
	Status          VendorRequestStatus
	MatchScore      *int
	MatchBreakdown  map[string]any
	CreatedAt       time.Time
	SentAt          *time.Time
}

// ResponseStatus is the vendor's business claim about the item.
// UNCLASSIFIED (PRD 2.1 §1-2) means the system could not safely determine
// what the vendor meant — it is not a synonym for UNAVAILABLE and must
// never be inferred from ambiguity, a bare price, or an extraction
// failure.
type ResponseStatus string

const (
	ResponseAvailable    ResponseStatus = "AVAILABLE"
	ResponseUnavailable  ResponseStatus = "UNAVAILABLE"
	ResponseCanSource    ResponseStatus = "CAN_SOURCE"
	ResponseAlternative  ResponseStatus = "ALTERNATIVE"
	ResponseUnclassified ResponseStatus = "UNCLASSIFIED"
)

// ReviewState is orthogonal to ResponseStatus (PRD 2.1 §1): it tracks
// whether a human needs to look at this response, not what the vendor
// said. A confident automatic classification is REVIEWED; anything the
// system can't safely classify — or can't safely associate with a
// VendorRequest — is NEEDS_REVIEW; an operator edit sets CORRECTED.
type ReviewState string

const (
	ReviewNeedsReview ReviewState = "NEEDS_REVIEW"
	ReviewReviewed    ReviewState = "REVIEWED"
	ReviewCorrected   ReviewState = "CORRECTED"
)

// ExtractionMethod records how a response's structured fields were
// produced (PRD 2.1 §8) — never fabricated; reflects what actually ran.
type ExtractionMethod string

const (
	ExtractionHeuristic ExtractionMethod = "heuristic"
	ExtractionAI        ExtractionMethod = "ai"
	ExtractionHybrid    ExtractionMethod = "hybrid"
	ExtractionManual    ExtractionMethod = "manual"
)

// VendorResponse is the vendor's structured reply. VendorRequestID is
// nullable (PRD 2.1 §4/§13): a response that cannot be safely associated
// with a specific VendorRequest is still persisted — attributed to the
// vendor via VendorID — rather than discarded or guessed at.
type VendorResponse struct {
	ID              uuid.UUID
	VendorRequestID *uuid.UUID
	VendorID        uuid.UUID
	Status          ResponseStatus
	ReviewState     ReviewState
	Product         *string
	Variant         *string
	Condition       *string
	Price           *int64
	Currency        string
	Quantity        *int
	Location        *string
	Notes           *string
	RawMessage      string
	ExtractedData   map[string]any
	Confidence      float64
	ExtractionMethod ExtractionMethod
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Repository is the persistence boundary for vendor requests + responses.
type Repository interface {
	CreateVendorRequest(ctx context.Context, vr *VendorRequest) (*VendorRequest, error)
	MarkSent(ctx context.Context, id uuid.UUID) error
	FindVendorRequestByID(ctx context.Context, id uuid.UUID) (*VendorRequest, error)
	FindVendorRequestByPublicReference(ctx context.Context, ref string) (*VendorRequest, error)
	ListVendorRequestsByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*VendorRequest, error)
	// ListActiveByVendor returns every VendorRequest for this vendor that
	// is still awaiting a response (PENDING or SENT, not yet RESPONDED/
	// EXPIRED/CANCELLED). The caller — not this method — decides what to
	// do when there is more than one (PRD 2.1 §4: multiple active requests
	// must never be silently collapsed into "the most recent one").
	ListActiveByVendor(ctx context.Context, vendorID uuid.UUID) ([]*VendorRequest, error)
	// ListByVendor returns every VendorRequest for this vendor regardless
	// of status — used by the dashboard's manual-association picker
	// (PRD 2.1 §5/§11).
	ListByVendor(ctx context.Context, vendorID uuid.UUID) ([]*VendorRequest, error)

	CreateVendorResponse(ctx context.Context, resp *VendorResponse) (*VendorResponse, error)
	UpdateVendorResponse(ctx context.Context, id uuid.UUID, fields ResponseFields) (*VendorResponse, error)
	FindResponseByID(ctx context.Context, id uuid.UUID) (*VendorResponse, error)
	FindResponseByVendorRequest(ctx context.Context, vendorRequestID uuid.UUID) (*VendorResponse, error)
	// ListUnmatched returns responses with no VendorRequest association
	// yet (PRD 2.1 §5/§13) — the operator's queue for manual association.
	ListUnmatched(ctx context.Context) ([]*VendorResponse, error)
	// UpdateAssociation sets/changes which VendorRequest a response
	// belongs to (PRD 2.1 §5). Returns the previous value so the caller
	// can write an accurate audit record.
	UpdateAssociation(ctx context.Context, responseID uuid.UUID, newVendorRequestID uuid.UUID) (previousVendorRequestID *uuid.UUID, updated *VendorResponse, err error)
}

// ResponseFields supports partial updates: nil means "leave unchanged".
// ReviewState and ExtractionMethod are included because a correction
// changes both.
type ResponseFields struct {
	Status           *ResponseStatus
	ReviewState      *ReviewState
	Product          *string
	Variant          *string
	Condition        *string
	Price            *int64
	Quantity         *int
	Location         *string
	Notes            *string
	ExtractionMethod *ExtractionMethod
}
