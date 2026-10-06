package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/media"
)

// MediaAssetRepository is the PostgreSQL implementation of media.Repository.
type MediaAssetRepository struct {
	db *sql.DB
}

func NewMediaAssetRepository(db *sql.DB) *MediaAssetRepository { return &MediaAssetRepository{db: db} }

const assetColumns = `id, owner_vendor_id, kind, mime_type, size_bytes, sha256, storage_key, source_message_id,
	provider, provider_media_id, provider_media_expires_at, status, stored_at, created_at`

type queryer interface {
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

func scanAsset(row interface{ Scan(...any) error }) (*media.Asset, error) {
	var (
		a                       media.Asset
		id, vendorID, sourceMsg string
		expires                 sql.NullTime
	)
	if err := row.Scan(&id, &vendorID, &a.Kind, &a.MimeType, &a.SizeBytes, &a.SHA256, &a.StorageKey, &sourceMsg,
		&a.Provider, &a.ProviderMediaID, &expires, &a.Status, &a.StoredAt, &a.CreatedAt); err != nil {
		return nil, err
	}
	a.ID, _ = uuid.Parse(id)
	a.OwnerVendorID, _ = uuid.Parse(vendorID)
	a.SourceMessageID, _ = uuid.Parse(sourceMsg)
	if expires.Valid {
		t := expires.Time
		a.ProviderMediaExpiresAt = &t
	}
	return &a, nil
}

func findByVendorSHA(ctx context.Context, q queryer, vendorID uuid.UUID, sha string) (*media.Asset, error) {
	a, err := scanAsset(q.QueryRowContext(ctx,
		`SELECT `+assetColumns+` FROM media_assets WHERE owner_vendor_id = $1 AND sha256 = $2`, vendorID.String(), sha))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func (r *MediaAssetRepository) FindByVendorAndSHA256(ctx context.Context, vendorID uuid.UUID, sha string) (*media.Asset, error) {
	return findByVendorSHA(ctx, r.db, vendorID, sha)
}

// FindByID returns an asset or nil (tests / operational inspection).
func (r *MediaAssetRepository) FindByID(ctx context.Context, id uuid.UUID) (*media.Asset, error) {
	a, err := scanAsset(r.db.QueryRowContext(ctx, `SELECT `+assetColumns+` FROM media_assets WHERE id = $1`, id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return a, err
}

func (r *MediaAssetRepository) MessageAssetID(ctx context.Context, messageID uuid.UUID) (*uuid.UUID, error) {
	var s sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT media_asset_id FROM messages WHERE id = $1`, messageID.String()).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !s.Valid) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	id, err := uuid.Parse(s.String)
	if err != nil {
		return nil, err
	}
	return &id, nil
}

func (r *MediaAssetRepository) LinkMessage(ctx context.Context, messageID, assetID uuid.UUID) error {
	res, err := r.db.ExecContext(ctx,
		`UPDATE messages SET media_asset_id = $2 WHERE id = $1 AND (media_asset_id IS NULL OR media_asset_id = $2)`,
		messageID.String(), assetID.String())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("message %s not found or already linked to another asset", messageID)
	}
	return nil
}

// StoreAndLink: see media.Repository. The insert is ON CONFLICT DO NOTHING
// against the (owner_vendor_id, sha256) and storage_key unique constraints,
// so concurrent or repeated calls converge on one row; the event is
// inserted only by the transaction that created the row.
func (r *MediaAssetRepository) StoreAndLink(ctx context.Context, a *media.Asset, messageID uuid.UUID, ev *event.Event) (*media.Asset, bool, error) {
	if a.ID == uuid.Nil {
		a.ID = uuid.New()
	}
	if a.Status == "" {
		a.Status = media.StatusAvailable
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()

	var expires any
	if a.ProviderMediaExpiresAt != nil {
		expires = *a.ProviderMediaExpiresAt
	}
	got, err := scanAsset(tx.QueryRowContext(ctx, `
		INSERT INTO media_assets (id, owner_vendor_id, kind, mime_type, size_bytes, sha256, storage_key,
		                          source_message_id, provider, provider_media_id, provider_media_expires_at, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT DO NOTHING
		RETURNING `+assetColumns,
		a.ID.String(), a.OwnerVendorID.String(), a.Kind, a.MimeType, a.SizeBytes, a.SHA256, a.StorageKey,
		a.SourceMessageID.String(), a.Provider, a.ProviderMediaID, expires, a.Status))
	created := err == nil
	switch {
	case errors.Is(err, sql.ErrNoRows):
		got, err = findByVendorSHA(ctx, tx, a.OwnerVendorID, a.SHA256)
		if err == nil && got == nil {
			err = fmt.Errorf("media asset conflict without a matching (vendor, sha256) row (storage key %s)", a.StorageKey)
		}
		if err != nil {
			return nil, false, err
		}
	case err != nil:
		return nil, false, err
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE messages SET media_asset_id = $2 WHERE id = $1 AND (media_asset_id IS NULL OR media_asset_id = $2)`,
		messageID.String(), got.ID.String())
	if err != nil {
		return nil, false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, false, fmt.Errorf("message %s not found or already linked to another asset", messageID)
	}

	if created && ev != nil {
		id := got.ID
		ev.EntityID = &id
		if err := insertEventTx(ctx, tx, ev); err != nil {
			return nil, false, fmt.Errorf("writing %s: %w", ev.EventType, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return got, created, nil
}

func insertEventTx(ctx context.Context, tx *sql.Tx, e *event.Event) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.Payload == nil {
		e.Payload = map[string]any{}
	}
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return err
	}
	var entityID, actorID any
	if e.EntityID != nil {
		entityID = e.EntityID.String()
	}
	if e.ActorID != nil {
		actorID = e.ActorID.String()
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO events (id, event_type, entity_type, entity_id, actor_type, actor_id, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, e.ID.String(), e.EventType, e.EntityType, entityID, e.ActorType, actorID, payload, e.CreatedAt)
	return err
}

func (r *MediaAssetRepository) UnlinkedVendorMedia(ctx context.Context, since time.Time, limit int) ([]media.UnlinkedMedia, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT v.id, m.id, m.content->'media'
		FROM messages m
		JOIN conversations c ON c.id = m.conversation_id AND c.channel = 'whatsapp'
		JOIN vendors v ON v.whatsapp_external_id = c.external_id AND v.status = 'ACTIVE'
		WHERE m.direction = 'INBOUND'
		  AND m.media_asset_id IS NULL
		  AND m.created_at >= $1
		  AND m.content->'media'->>'kind' IN ('image', 'video')
		  AND COALESCE(m.content->'media'->>'provider_ref', '') <> ''
		  AND NOT EXISTS (SELECT 1 FROM jobs j WHERE j.job_type = 'MEDIA_DOWNLOAD' AND j.idempotency_key = m.id::text)
		ORDER BY m.created_at
		LIMIT $2
	`, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []media.UnlinkedMedia
	for rows.Next() {
		var vid, mid string
		var raw []byte
		if err := rows.Scan(&vid, &mid, &raw); err != nil {
			return nil, err
		}
		var u media.UnlinkedMedia
		u.VendorID, _ = uuid.Parse(vid)
		u.MessageID, _ = uuid.Parse(mid)
		var ref conversation.MediaRef
		if err := json.Unmarshal(raw, &ref); err != nil {
			continue
		}
		u.Ref = ref
		out = append(out, u)
	}
	return out, rows.Err()
}

// OutboundRef returns the cached outbound provider media ID of an asset
// (Phase 4B media reuse), "" if none.
func (r *MediaAssetRepository) OutboundRef(ctx context.Context, id uuid.UUID) (string, *time.Time, error) {
	var ref sql.NullString
	var exp sql.NullTime
	err := r.db.QueryRowContext(ctx, `SELECT outbound_provider_ref, outbound_ref_expires_at FROM media_assets WHERE id = $1`, id.String()).Scan(&ref, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	if !exp.Valid {
		return ref.String, nil, nil
	}
	t := exp.Time
	return ref.String, &t, nil
}

// SetOutboundRef caches the provider media ID an asset was uploaded as.
func (r *MediaAssetRepository) SetOutboundRef(ctx context.Context, id uuid.UUID, ref string, expiresAt time.Time) error {
	_, err := r.db.ExecContext(ctx, `UPDATE media_assets SET outbound_provider_ref = $2, outbound_ref_expires_at = $3 WHERE id = $1`,
		id.String(), ref, expiresAt)
	return err
}
