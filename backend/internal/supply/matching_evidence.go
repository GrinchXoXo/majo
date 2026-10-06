package supply

import (
	"context"

	"github.com/google/uuid"
)

// MatchingEvidence is the curated supply matching may use (Phase 4A, Lock
// V2 §M-4A.6): each vendor's ACTIVE, unexpired SupplyListings, plus the
// signals a curation decision already covers. A signal whose listing was
// published is represented by that listing (with the operator's
// corrections); a signal whose listing was rejected is no longer evidence.
type MatchingEvidence struct {
	ListingsByVendor  map[uuid.UUID][]MatchableListing
	SupersededSignals map[uuid.UUID]bool
}

// MatchableListing is an ACTIVE listing plus its source signal's product
// in compact search form. Matching accepts either: an operator's display
// correction ("iphone 13" -> "Apple iPhone 13 128GB") must not make the
// same supply match a request for "iPhone 13" less well.
type MatchableListing struct {
	Listing       *Listing
	SourceProduct string // NormalizeSearch(signal.product); "" if unknown
	// SourceAvailability is the source signal's availability. A listing
	// built from a CAN_SOURCE signal is still "can source" supply when
	// published; publication curates it, it does not upgrade it.
	SourceAvailability Availability
}

// MatchingEvidenceReader loads MatchingEvidence for a set of vendors.
type MatchingEvidenceReader interface {
	MatchingEvidence(ctx context.Context, vendorIDs []uuid.UUID) (*MatchingEvidence, error)
}
