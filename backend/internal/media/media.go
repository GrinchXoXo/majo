// Package media owns Majo's durable media: the MediaAsset (Architecture Lock
// V2 §I). A conversation.MediaRef is the provider's temporary reference to
// something a sender attached; a MediaAsset is Majo's own stored copy,
// owned by a vendor and traceable to the message that produced it.
//
// Phase 4A.1 scope: ingestion only (MediaRef -> durable job -> download ->
// validate -> SHA-256 -> vendor-scoped dedupe -> storage.Provider ->
// MediaAsset -> MEDIA_ASSET_STORED). Stored is not published: nothing here
// makes media visible to buyers, and storing it implies no vendor consent
// to redistribution.
package media

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
)

// Kind of media asset. 4A.1 ingests only what the supply use case needs.
type Kind string

const (
	KindImage Kind = "image"
	KindVideo Kind = "video"
)

// Status of a MediaAsset. Only AVAILABLE is written in 4A.1; REMOVED is
// reserved (moderation/deletion is not implemented).
type Status string

const (
	StatusAvailable Status = "AVAILABLE"
	StatusRemoved   Status = "REMOVED"
)

// Asset is the MediaAsset: Majo's durable, vendor-owned media record.
//
// Three identities that must not be confused:
//   - ID: Majo's durable record.
//   - SHA256: the content (hex of the actual stored bytes).
//   - ProviderMediaID: the transport reference that produced it (expires).
type Asset struct {
	ID              uuid.UUID
	OwnerVendorID   uuid.UUID
	Kind            Kind
	MimeType        string
	SizeBytes       int64
	SHA256          string
	StorageKey      string
	SourceMessageID uuid.UUID // first inbound message that produced these bytes for this vendor
	Provider        string
	ProviderMediaID string
	// ProviderMediaExpiresAt is nil unless the provider states an expiry.
	// WhatsApp does not, so it is nil for WhatsApp media (never guessed).
	ProviderMediaExpiresAt *time.Time
	Status                 Status
	StoredAt               time.Time
	CreatedAt              time.Time
}

// Repository persists MediaAssets.
type Repository interface {
	// FindByVendorAndSHA256 returns the vendor's asset with these bytes, or nil.
	FindByVendorAndSHA256(ctx context.Context, vendorID uuid.UUID, sha256 string) (*Asset, error)
	// MessageAssetID returns the asset an inbound message is already linked
	// to (messages.media_asset_id), or nil.
	MessageAssetID(ctx context.Context, messageID uuid.UUID) (*uuid.UUID, error)
	// StoreAndLink, in ONE database transaction: inserts the asset unless
	// the vendor already owns an asset with the same SHA-256; links the
	// message to whichever asset holds the bytes; and, only if the asset
	// row was created by this call, writes storedEvent (its EntityID is set
	// to the asset ID). Returns the asset that holds the bytes and whether
	// it was created here. Safe to call repeatedly and concurrently.
	StoreAndLink(ctx context.Context, a *Asset, messageID uuid.UUID, storedEvent *event.Event) (*Asset, bool, error)
	// LinkMessage sets messages.media_asset_id (dedupe hit, no new asset).
	LinkMessage(ctx context.Context, messageID, assetID uuid.UUID) error
	// UnlinkedVendorMedia lists inbound image/video messages from currently
	// active vendors, received since `since`, that have no asset and no
	// download job yet. Used once at startup to close the gap between
	// "message saved" and "job enqueued" (crash in between), and to pick up
	// media persisted before durable ingestion existed.
	UnlinkedVendorMedia(ctx context.Context, since time.Time, limit int) ([]UnlinkedMedia, error)
}

// UnlinkedMedia is an inbound vendor media message without an asset or job.
type UnlinkedMedia struct {
	VendorID  uuid.UUID
	MessageID uuid.UUID
	Ref       conversation.MediaRef
}

// Downloader fetches provider media bytes by the provider's media ID. It
// hides every provider-specific detail (endpoints, auth, URL expiry).
// Implementations must refuse to return more than maxBytes, and should
// mark non-retryable failures with a Permanent() bool method.
type Downloader interface {
	DownloadMedia(ctx context.Context, providerMediaID string, maxBytes int64) (data []byte, mimeType string, err error)
}

// Error reasons recorded on terminal failures (stable strings for operators).
var (
	ErrUnsupportedKind = errors.New("media: unsupported media kind")
	ErrUnsupportedMIME = errors.New("media: unsupported or unrecognised content type")
	ErrTooLarge        = errors.New("media: payload exceeds maximum allowed size")
	ErrEmpty           = errors.New("media: empty payload")
	ErrKindMismatch    = errors.New("media: content does not match the declared media kind")
	ErrNoProvider      = errors.New("media: no downloader for provider")
)
