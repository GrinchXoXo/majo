package media

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/storage"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
)

// JobTypeDownload is the durable job type for inbound media ingestion.
const JobTypeDownload = "MEDIA_DOWNLOAD"

// Enqueuer is the narrow interface the WhatsApp adapter uses to hand off
// inbound vendor media. It only records a durable job; it never downloads.
type Enqueuer interface {
	EnqueueDownload(ctx context.Context, vendorID, messageID uuid.UUID, ref *conversation.MediaRef) error
}

// Ingestor turns MediaRefs into MediaAssets through the durable job queue.
type Ingestor struct {
	jobs        jobs.Store
	assets      Repository
	store       storage.Provider
	downloaders map[string]Downloader // by MediaRef.Provider (e.g. "whatsapp")
	events      event.Writer
	limits      Limits
	maxAttempts int
	logger      *slog.Logger
}

func NewIngestor(js jobs.Store, assets Repository, store storage.Provider, downloaders map[string]Downloader,
	events event.Writer, limits Limits, maxAttempts int, logger *slog.Logger) *Ingestor {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return &Ingestor{jobs: js, assets: assets, store: store, downloaders: downloaders, events: events,
		limits: limits, maxAttempts: maxAttempts, logger: logger}
}

// downloadPayload is the job payload. It carries everything the worker
// needs, so a job is self-contained even if the process restarts.
type downloadPayload struct {
	MessageID       uuid.UUID
	VendorID        uuid.UUID
	Provider        string
	ProviderMediaID string
	Kind            Kind
	DeclaredMIME    string
}

func (p downloadPayload) toMap() map[string]any {
	return event.Merge(map[string]any{
		"message_id": p.MessageID.String(), "vendor_id": p.VendorID.String(), "provider": p.Provider,
		"provider_media_id": p.ProviderMediaID, "kind": string(p.Kind), "declared_mime_type": p.DeclaredMIME,
	})
}

func parsePayload(m map[string]any) (downloadPayload, error) {
	str := func(k string) string { s, _ := m[k].(string); return s }
	var p downloadPayload
	var err error
	if p.MessageID, err = uuid.Parse(str("message_id")); err != nil {
		return p, fmt.Errorf("invalid message_id: %w", err)
	}
	if p.VendorID, err = uuid.Parse(str("vendor_id")); err != nil {
		return p, fmt.Errorf("invalid vendor_id: %w", err)
	}
	p.Provider, p.ProviderMediaID, p.Kind, p.DeclaredMIME = str("provider"), str("provider_media_id"), Kind(str("kind")), str("declared_mime_type")
	if p.Provider == "" || p.ProviderMediaID == "" {
		return p, errors.New("missing provider or provider_media_id")
	}
	return p, nil
}

// EnqueueDownload records a durable MEDIA_DOWNLOAD job for an inbound
// vendor media message. Idempotent per message: a redelivered webhook (or
// the startup backfill) cannot create a second job. Kinds 4A.1 does not
// ingest (audio, document, sticker) are skipped and stay MediaRef-only.
func (s *Ingestor) EnqueueDownload(ctx context.Context, vendorID, messageID uuid.UUID, ref *conversation.MediaRef) error {
	if ref == nil || ref.ProviderRef == "" {
		return nil
	}
	if !SupportedKind(ref.Kind) {
		s.logger.Info("media kind not ingested in 4A.1; kept as MediaRef only",
			"kind", ref.Kind, "message_id", messageID, "vendor_id", vendorID)
		return nil
	}
	p := downloadPayload{MessageID: messageID, VendorID: vendorID, Provider: ref.Provider,
		ProviderMediaID: ref.ProviderRef, Kind: Kind(ref.Kind), DeclaredMIME: ref.MimeType}
	created, err := s.jobs.Enqueue(ctx, &jobs.Job{
		Type: JobTypeDownload, IdempotencyKey: messageID.String(), Payload: p.toMap(), MaxAttempts: s.maxAttempts,
	})
	if err != nil {
		return fmt.Errorf("enqueueing media download: %w", err)
	}
	s.logger.Info("media download job enqueued", "created", created, "message_id", messageID,
		"vendor_id", vendorID, "provider_media_id", ref.ProviderRef, "kind", ref.Kind)
	return nil
}

// Backfill enqueues download jobs for vendor media messages received since
// `since` that have neither an asset nor a job (at most limit; see
// Repository.UnlinkedVendorMedia). Idempotent; returns how many it enqueued.
func (s *Ingestor) Backfill(ctx context.Context, since time.Time, limit int) (int, error) {
	missing, err := s.assets.UnlinkedVendorMedia(ctx, since, limit)
	if err != nil {
		return 0, fmt.Errorf("listing unlinked vendor media: %w", err)
	}
	for i, m := range missing {
		ref := m.Ref
		if err := s.EnqueueDownload(ctx, m.VendorID, m.MessageID, &ref); err != nil {
			return i, err
		}
	}
	if len(missing) == limit {
		s.logger.Warn("media backfill hit its limit; remaining messages are picked up on the next start", "limit", limit)
	}
	return len(missing), nil
}

// Handle is the MEDIA_DOWNLOAD job handler:
//
//	download -> validate -> SHA-256 -> vendor-scoped dedupe -> storage.Put
//	  -> (one DB tx) MediaAsset + message link + MEDIA_ASSET_STORED
//
// Every step is safe to repeat (at-least-once execution):
//   - a message already linked to an asset short-circuits (no re-download);
//   - the storage key is deterministic per (vendor, sha256), so a repeated
//     Put overwrites the same object rather than creating another;
//   - the (owner_vendor_id, sha256) unique constraint makes the insert
//     idempotent, and the event is written only by the call that created
//     the row, in the same transaction, so it means "stored and retrievable".
func (s *Ingestor) Handle(ctx context.Context, j *jobs.Job) (err error) {
	p, err := parsePayload(j.Payload)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("malformed media job payload: %w", err))
	}
	log := s.logger.With("job_id", j.ID, "attempt", j.Attempts, "max_attempts", j.MaxAttempts, "message_id", p.MessageID,
		"vendor_id", p.VendorID, "provider_media_id", p.ProviderMediaID, "kind", p.Kind)
	defer func() {
		if err != nil {
			// The worker logs the retry/terminal decision; this line carries
			// the media identifiers needed to debug it. No tokens or URLs.
			log.Warn("media ingestion attempt failed", "error", err, "permanent", jobs.IsPermanent(err))
		}
	}()

	if linked, err := s.assets.MessageAssetID(ctx, p.MessageID); err != nil {
		return fmt.Errorf("checking existing message asset: %w", err)
	} else if linked != nil {
		log.Info("message already has a media asset; nothing to do", "media_asset_id", *linked)
		return nil
	}

	max := s.limits.MaxBytes(p.Kind)
	if max <= 0 {
		return jobs.Permanent(fmt.Errorf("%w: %q", ErrUnsupportedKind, p.Kind))
	}
	dl := s.downloaders[p.Provider]
	if dl == nil {
		return jobs.Permanent(fmt.Errorf("%w %q", ErrNoProvider, p.Provider))
	}

	data, _, err := dl.DownloadMedia(ctx, p.ProviderMediaID, max)
	if err != nil {
		return fmt.Errorf("downloading provider media: %w", err) // provider marks permanent ones
	}
	v, err := Validate(p.Kind, data, s.limits)
	if err != nil {
		return jobs.Permanent(err)
	}
	log = log.With("sha256", v.SHA256, "size", v.Size, "mime_type", v.MimeType)

	if existing, err := s.assets.FindByVendorAndSHA256(ctx, p.VendorID, v.SHA256); err != nil {
		return fmt.Errorf("checking vendor asset dedupe: %w", err)
	} else if existing != nil {
		if err := s.assets.LinkMessage(ctx, p.MessageID, existing.ID); err != nil {
			return fmt.Errorf("linking message to existing asset: %w", err)
		}
		log.Info("media bytes already stored for this vendor; message linked to existing asset", "media_asset_id", existing.ID)
		return nil
	}

	key := StorageKey(p.VendorID, v.SHA256, v.Extension)
	if err := s.store.Put(ctx, key, data, v.MimeType); err != nil {
		return fmt.Errorf("storing media: %w", err)
	}

	a := &Asset{
		ID: uuid.New(), OwnerVendorID: p.VendorID, Kind: p.Kind, MimeType: v.MimeType, SizeBytes: v.Size,
		SHA256: v.SHA256, StorageKey: key, SourceMessageID: p.MessageID, Provider: p.Provider,
		ProviderMediaID: p.ProviderMediaID, Status: StatusAvailable,
	}
	vendorID := p.VendorID
	stored := &event.Event{
		EventType: event.MediaAssetStored, EntityType: "media_asset", ActorType: event.ActorVendor, ActorID: &vendorID,
		Payload: event.Merge(map[string]any{
			"media_asset_id": a.ID.String(), "source_message_id": p.MessageID.String(), "vendor_id": p.VendorID.String(),
			"kind": string(a.Kind), "mime_type": a.MimeType, "size_bytes": a.SizeBytes, "sha256": a.SHA256,
			"provider": a.Provider, "provider_media_id": a.ProviderMediaID, "job_id": j.ID.String(), "channel": p.Provider,
		}),
	}
	got, created, err := s.assets.StoreAndLink(ctx, a, p.MessageID, stored)
	if err != nil {
		// The object may already be in storage; a retry targets the same
		// deterministic key, so it is overwritten rather than duplicated.
		return fmt.Errorf("persisting media asset: %w", err)
	}
	if created {
		log.Info("media asset stored", "media_asset_id", got.ID, "storage_key", got.StorageKey)
	} else {
		log.Info("media asset already existed (concurrent ingestion); message linked", "media_asset_id", got.ID)
	}
	return nil
}

// OnFailure records MEDIA_INGESTION_FAILED after a job fails terminally,
// so operators can see "failed after N attempts" alongside the message.
func (s *Ingestor) OnFailure(ctx context.Context, j *jobs.Job, cause error) {
	p, _ := parsePayload(j.Payload)
	var entityID *uuid.UUID
	if p.MessageID != uuid.Nil {
		m := p.MessageID
		entityID = &m
	}
	if err := s.events.Write(ctx, &event.Event{
		EventType: event.MediaIngestionFailed, EntityType: "message", EntityID: entityID,
		ActorType: event.ActorSystem,
		Payload: event.Merge(map[string]any{
			"job_id": j.ID.String(), "source_message_id": j.Payload["message_id"], "vendor_id": j.Payload["vendor_id"],
			"provider": p.Provider, "provider_media_id": p.ProviderMediaID, "kind": string(p.Kind),
			"attempts": j.Attempts, "max_attempts": j.MaxAttempts, "permanent": jobs.IsPermanent(cause),
			"error": j.LastError, "channel": p.Provider,
		}),
	}); err != nil {
		s.logger.Error("writing MEDIA_INGESTION_FAILED event", "job_id", j.ID, "error", err)
	}
}
