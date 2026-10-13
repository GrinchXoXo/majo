package supply

// Phase 4A.3: SupplySignal -> SupplyListing construction.
//
// A SupplySignal is evidence of what a vendor sent; a SupplyListing is the
// durable marketplace record built from it. This file is the one canonical
// path between them (ListingService.CreateFromSignal). It:
//
//   - accepts only eligible signals (active vendor, a product, available or
//     can-source, not expired);
//   - copies the extracted fields as they are (nothing is invented);
//   - resolves location: the signal's own location, else the vendor's
//     primary location, else none;
//   - references the signal's stored MediaAssets in evidence order (no copy,
//     no re-download);
//   - stamps last_verified_at with the time the evidence arrived and a
//     provisional expires_at = that + the lifecycle ceiling (Phase 4A.5:
//     a draft's lifecycle has not started; publication restarts the
//     ceiling from published_at — see eligibility.go);
//   - always creates a DRAFT (publishing stays an operator action);
//   - is idempotent and race-safe (see ListingRepository.CreateFromSignal).

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/vendor"
)

// JobTypeListingDraft is the durable job that builds a DRAFT listing for a
// new (or newly media-complete) SupplySignal.
const JobTypeListingDraft = "SUPPLY_LISTING_DRAFT"

// ErrIneligible: the signal cannot become a listing. Final, not retried.
type ErrIneligible struct{ Reason string }

func (e ErrIneligible) Error() string { return "supply signal not eligible for a listing: " + e.Reason }

// ErrSignalNotFound: no such signal.
var ErrSignalNotFound = errors.New("supply signal not found")

// VendorLookup is the vendor read the listing path needs (vendor.Repository satisfies it).
type VendorLookup interface {
	FindByID(ctx context.Context, id uuid.UUID) (*vendor.Vendor, error)
}

// SignalMediaReader returns the media a signal's evidence holds, in
// evidence order and de-duplicated: usable references for AVAILABLE
// assets, and the IDs of referenced assets that are not usable.
type SignalMediaReader interface {
	SignalMedia(ctx context.Context, signalID uuid.UUID) (refs []MediaReference, unusable []uuid.UUID, firstMessageID *uuid.UUID, err error)
	// MediaAssetStates (Phase 4A Step 4) returns the state of each existing
	// asset among ids; missing assets are absent from the map.
	MediaAssetStates(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]MediaAssetState, error)
}

// NormalizeSearch is the deterministic compact search form used for listing
// products and locations: lowercase, letters and digits only.
//
//	"iPhone 13" / "iphone13" / "IPHONE-13" -> "iphone13"
//	"Benin City" / "BENIN CITY"           -> "benincity"
//
// Display values are never replaced by it.
func NormalizeSearch(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// publicRefAlphabet is Crockford base32 (no I, L, O, U): unambiguous when
// read aloud or typed by a buyer.
const publicRefAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// NewPublicReference returns a random "L-XXXXXX" (32^6 ~ 1.07e9 values).
// Uniqueness is enforced by the database; callers retry on collision.
func NewPublicReference() (string, error) {
	b := make([]byte, 6)
	max := big.NewInt(int64(len(publicRefAlphabet)))
	for i := range b {
		n, err := rand.Int(rand.Reader, max)
		if err != nil {
			return "", err
		}
		b[i] = publicRefAlphabet[n.Int64()]
	}
	return "L-" + string(b), nil
}

// eligibility applies the listing rules to a signal (pure).
func eligibility(sig *Signal, v *vendor.Vendor, now time.Time) error {
	switch {
	case v == nil:
		return ErrIneligible{"vendor not found"}
	case v.Status != vendor.StatusActive:
		return ErrIneligible{"vendor is not ACTIVE (" + string(v.Status) + ")"}
	case sig.Product == nil || strings.TrimSpace(*sig.Product) == "" || NormalizeSearch(*sig.Product) == "":
		return ErrIneligible{"signal has no product identity"}
	case sig.AvailabilityStatus != AvailabilityAvailable && sig.AvailabilityStatus != AvailabilityCanSource:
		return ErrIneligible{"signal availability is " + string(sig.AvailabilityStatus)}
	case !sig.ExpiresAt.IsZero() && !sig.ExpiresAt.After(now):
		return ErrIneligible{"signal evidence has expired"}
	}
	if inc, _ := sig.ExtractedData["incomplete"].(bool); inc {
		return ErrIneligible{"signal is incomplete (media without text)"}
	}
	return nil
}

// Location sources (recorded in the listing event, not in a column).
const (
	LocationFromSignal        = "signal"
	LocationFromVendorPrimary = "vendor_primary_location"
	LocationNone              = "none"
)

// resolveLocation: the signal's explicit location, else the vendor's
// primary location, else nothing. Service areas are not a location.
func resolveLocation(sig *Signal, v *vendor.Vendor) (*string, string) {
	if sig.Location != nil && strings.TrimSpace(*sig.Location) != "" {
		l := *sig.Location
		return &l, LocationFromSignal
	}
	if v != nil && strings.TrimSpace(v.PrimaryLocation) != "" {
		l := v.PrimaryLocation
		return &l, LocationFromVendorPrimary
	}
	return nil, LocationNone
}

// buildListing maps an eligible signal onto a DRAFT listing (pure).
func buildListing(sig *Signal, v *vendor.Vendor, media []MediaReference, firstMessage *uuid.UUID, policy *EligibilityPolicy) (*Listing, string) {
	loc, locSource := resolveLocation(sig, v)
	norm := NormalizeSearch(*sig.Product)
	verified := sig.CreatedAt
	if verified.IsZero() {
		verified = time.Now().UTC()
	}
	sigID := sig.ID
	currency := sig.Currency
	if currency == "" {
		currency = "NGN"
	}
	return &Listing{
		VendorID: sig.VendorID, SupplySignalID: &sigID,
		Category: sig.Category, Product: sig.Product, NormalizedProduct: &norm, Variant: sig.Variant,
		Condition: sig.Condition, Price: sig.Price, Currency: currency, Quantity: sig.Quantity, Location: loc,
		MediaReferences: media, Status: ListingDraft, SourceChannel: "whatsapp", SourceMessageID: firstMessage,
		ExtractionMethod: "heuristic", Confidence: sig.Confidence,
		LastVerifiedAt: &verified, ExpiresAt: policy.CeilingFrom(verified),
	}, locSource
}

func mediaIDs(refs []MediaReference) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.AssetID
	}
	return out
}

// CreateFromSignal is the canonical SupplySignal -> SupplyListing operation.
// The result is always a DRAFT (or the existing/refreshed listing); it never
// publishes. actor is OPERATOR when an operator triggered it, SYSTEM for the
// automatic draft job.
func (s *ListingService) CreateFromSignal(ctx context.Context, signalID uuid.UUID, actor event.ActorType, actorID *uuid.UUID) (*Listing, ListingOutcome, error) {
	sig, err := s.signals.FindByID(ctx, signalID)
	if err != nil {
		return nil, "", fmt.Errorf("loading supply signal: %w", err)
	}
	if sig == nil {
		return nil, "", ErrSignalNotFound
	}
	if s.vendors == nil || s.media == nil {
		return nil, "", errors.New("listing service is missing its vendor/media sources")
	}
	v, err := s.vendors.FindByID(ctx, sig.VendorID)
	if err != nil {
		return nil, "", fmt.Errorf("loading vendor: %w", err)
	}
	if err := eligibility(sig, v, time.Now()); err != nil {
		return nil, "", err
	}
	refs, unusable, firstMsg, err := s.media.SignalMedia(ctx, signalID)
	if err != nil {
		return nil, "", fmt.Errorf("loading signal media: %w", err)
	}
	l, locSource := buildListing(sig, v, refs, firstMsg, s.policy)

	base := map[string]any{
		"supply_signal_id": signalID.String(), "vendor_id": sig.VendorID.String(), "channel": "whatsapp",
	}
	listed, outcome, err := s.listings.CreateFromSignal(ctx, ListingWrite{
		Listing: l,
		OnCreate: func(c *Listing) *event.Event {
			p := event.Merge(base, map[string]any{
				"supply_listing_id": c.ID.String(), "public_reference": c.PublicReference, "status": string(c.Status),
				"product": c.Product, "category": c.Category, "variant": c.Variant, "condition": c.Condition,
				"price": c.Price, "currency": c.Currency, "quantity": c.Quantity,
				"location": c.Location, "location_source": locSource,
				"last_verified_at": c.LastVerifiedAt.UTC().Format(time.RFC3339), "expires_at": c.ExpiresAt.UTC().Format(time.RFC3339),
			})
			p["media_asset_ids"] = mediaIDs(c.MediaReferences)
			if len(unusable) > 0 {
				p["unusable_media_asset_ids"] = uuidStrings(unusable)
			}
			id := c.ID
			return &event.Event{EventType: event.SupplyListingCreated, EntityType: "supply_listing", EntityID: &id,
				ActorType: actor, ActorID: actorID, Payload: p}
		},
		OnRefresh: func(e *Listing) *event.Event {
			id := e.ID
			// A duplicate refresh is a verification (new vendor evidence of
			// the same supply): it moves last_verified_at only. The lifecycle
			// ceiling (expires_at) is unchanged (Phase 4A.5).
			p := event.Merge(base, map[string]any{"supply_listing_id": e.ID.String(), "public_reference": e.PublicReference,
				"last_verified_at": e.LastVerifiedAt.UTC().Format(time.RFC3339), "expires_at": e.ExpiresAt.UTC().Format(time.RFC3339),
				"verification": "vendor_resubmission"})
			// The new signal's media is recorded, not merged (see report).
			p["signal_media_asset_ids"] = mediaIDs(refs)
			return &event.Event{EventType: event.SupplyListingDuplicateRefreshed, EntityType: "supply_listing", EntityID: &id,
				ActorType: actor, ActorID: actorID, Payload: p}
		},
		OnMediaSync: func(e *Listing, added []MediaReference) *event.Event {
			id := e.ID
			p := event.Merge(base, map[string]any{"supply_listing_id": e.ID.String(), "change": "media_added_from_signal"})
			p["added_media_asset_ids"] = mediaIDs(added)
			return &event.Event{EventType: event.SupplyListingUpdated, EntityType: "supply_listing", EntityID: &id,
				ActorType: actor, ActorID: actorID, Payload: p}
		},
	})
	if err != nil {
		return nil, "", fmt.Errorf("persisting listing: %w", err)
	}
	s.logger.Info("supply listing from signal", "outcome", outcome, "supply_signal_id", signalID,
		"supply_listing_id", listed.ID, "public_reference", listed.PublicReference, "status", listed.Status,
		"location_source", locSource, "media_asset_ids", mediaIDs(listed.MediaReferences))
	return listed, outcome, nil
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = id.String()
	}
	return out
}

// DraftListener is told when a DRAFT listing exists for a vendor's signal
// (the autonomous runtime evaluates trusted-vendor auto-publishing).
type DraftListener interface {
	DraftReady(ctx context.Context, l *Listing)
}

// SetDraftListener wires the autonomous runtime.
func (s *ListingService) SetDraftListener(l DraftListener) { s.draftListener = l }

// SetDraftJobs enables automatic DRAFT creation (composition root).
func (s *ListingService) SetDraftJobs(js jobs.Store, maxAttempts int) {
	s.jobs, s.maxAttempts = js, maxAttempts
}

// SignalChanged implements SignalListener: a signal was created or gained
// evidence, so (re)evaluate its DRAFT listing on the durable worker. key
// distinguishes successive changes of the same signal.
func (s *ListingService) SignalChanged(ctx context.Context, signalID uuid.UUID, key string) error {
	if s.jobs == nil {
		return nil
	}
	_, err := s.jobs.Enqueue(ctx, &jobs.Job{
		Type: JobTypeListingDraft, IdempotencyKey: signalID.String() + ":" + key, MaxAttempts: s.maxAttempts,
		Payload: map[string]any{"supply_signal_id": signalID.String(), "change": key},
	})
	if err != nil {
		return fmt.Errorf("enqueueing listing draft: %w", err)
	}
	return nil
}

// HandleDraftJob is the SUPPLY_LISTING_DRAFT handler. An ineligible signal
// is a normal outcome (no listing), not a failure.
func (s *ListingService) HandleDraftJob(ctx context.Context, j *jobs.Job) error {
	idStr, _ := j.Payload["supply_signal_id"].(string)
	id, err := uuid.Parse(idStr)
	if err != nil {
		return jobs.Permanent(fmt.Errorf("malformed listing draft payload: %w", err))
	}
	l, _, err := s.CreateFromSignal(ctx, id, event.ActorSystem, nil)
	if err == nil && l != nil && l.Status == ListingDraft && s.draftListener != nil {
		s.draftListener.DraftReady(ctx, l)
	}
	var inel ErrIneligible
	switch {
	case errors.As(err, &inel):
		s.logger.Info("supply signal not turned into a listing", "supply_signal_id", id, "reason", inel.Reason)
		return nil
	case errors.Is(err, ErrSignalNotFound):
		return jobs.Permanent(err)
	}
	return err
}
