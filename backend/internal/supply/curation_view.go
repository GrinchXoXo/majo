package supply

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// CurationView is the operator's read model of one listing (Phase 4A Step
// 4): the marketplace fields, and next to them, unchanged, the evidence the
// listing came from. Read-only; it never feeds a write.
type CurationView struct {
	Listing *Listing
	Vendor  *CurationVendor // nil if the vendor row is gone
	Signal  *CurationSignal // nil if the listing has no (existing) source signal
	Sources []CurationSource
	Media   []CurationMedia
}

type CurationVendor struct {
	ID              uuid.UUID
	BusinessName    string
	Status          string
	PrimaryLocation string
}

// CurationSignal is the source SupplySignal exactly as stored (evidence).
type CurationSignal struct {
	ID                 uuid.UUID
	Category           *string
	Product            *string
	Variant            *string
	Condition          *string
	Price              *int64
	Currency           string
	Quantity           *int
	Location           *string
	AvailabilityStatus string
	RawMessage         string
	Confidence         float64
	CreatedAt          time.Time
	ExpiresAt          time.Time
}

// CurationSource is one evidence message of the source signal, in order.
type CurationSource struct {
	Position          int
	MessageID         uuid.UUID
	ExternalMessageID *string
	MessageType       string
	Text              *string
	MediaAssetID      *uuid.UUID
	ReceivedAt        time.Time
}

// CurationMedia is one listing media reference joined with its asset.
type CurationMedia struct {
	Order     int
	AssetID   string
	Type      string
	Role      string
	Primary   bool // the effective primary image (see PrimaryMedia)
	Exists    bool // the MediaAsset row exists and belongs to the vendor
	Status    string
	MimeType  string
	SizeBytes int64
}

// Available reports whether the media can be previewed.
func (m CurationMedia) Available() bool { return m.Exists && m.Status == "AVAILABLE" }

// CurationReader loads CurationViews. Returns (nil, nil) for an unknown listing.
type CurationReader interface {
	CurationView(ctx context.Context, listingID uuid.UUID) (*CurationView, error)
}
