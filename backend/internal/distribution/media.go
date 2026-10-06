package distribution

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/channel"
	"github.com/middleman/backend/internal/media"
)

// AssetStore is what the publisher needs from MediaAsset persistence.
type AssetStore interface {
	FindByID(ctx context.Context, id uuid.UUID) (*media.Asset, error)
	OutboundRef(ctx context.Context, id uuid.UUID) (ref string, expiresAt *time.Time, err error)
	SetOutboundRef(ctx context.Context, id uuid.UUID, ref string, expiresAt time.Time) error
}

// Blobs reads stored bytes (storage.Provider implements it).
type Blobs interface {
	Get(ctx context.Context, key string) ([]byte, error)
}

// MediaPublisher turns a stored MediaAsset into a channel media reference.
// The asset is the single stored copy: it is uploaded to the channel once
// and the provider's outbound media ID is reused until it nears expiry. No
// per-delivery copies, no re-download from the vendor, no transcoding.
type MediaPublisher struct {
	assets   AssetStore
	blobs    Blobs
	uploader channel.MediaUploader
	ttl      time.Duration
	now      func() time.Time
}

// NewMediaPublisher builds a publisher.
func NewMediaPublisher(assets AssetStore, blobs Blobs, uploader channel.MediaUploader, ttl time.Duration) *MediaPublisher {
	return &MediaPublisher{assets: assets, blobs: blobs, uploader: uploader, ttl: ttl, now: time.Now}
}

// ErrAssetUnavailable: the asset is missing or removed.
var ErrAssetUnavailable = errors.New("media asset unavailable")

// Publish fills ref.ProviderRef.
func (p *MediaPublisher) Publish(ctx context.Context, ref *channel.MediaRef) error {
	if ref == nil {
		return nil
	}
	now := p.now().UTC()
	if cached, exp, err := p.assets.OutboundRef(ctx, ref.AssetID); err == nil && cached != "" && exp != nil && now.Before(*exp) {
		ref.ProviderRef = cached
		return nil
	}
	a, err := p.assets.FindByID(ctx, ref.AssetID)
	if err != nil {
		return fmt.Errorf("loading media asset: %w", err)
	}
	if a == nil || a.Status != media.StatusAvailable {
		return ErrAssetUnavailable
	}
	data, err := p.blobs.Get(ctx, a.StorageKey)
	if err != nil {
		return fmt.Errorf("reading stored media: %w", err)
	}
	id, err := p.uploader.UploadMedia(ctx, data, a.MimeType, a.ID.String()+extensionFor(a.MimeType))
	if err != nil {
		return fmt.Errorf("uploading media to channel: %w", err)
	}
	if err := p.assets.SetOutboundRef(ctx, a.ID, id, now.Add(p.ttl)); err != nil {
		return fmt.Errorf("caching outbound media id: %w", err)
	}
	ref.ProviderRef = id
	return nil
}

func extensionFor(mime string) string {
	switch mime {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/webp":
		return ".webp"
	case "video/mp4":
		return ".mp4"
	case "video/3gpp":
		return ".3gp"
	}
	return ""
}
