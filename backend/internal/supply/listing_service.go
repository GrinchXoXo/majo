package supply

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/event"
)

// ListingService turns a vendor-submitted SupplySignal into durable,
// discoverable marketplace supply (PRD 3 §5-6). Publishing is an
// explicit operator action, not automatic — a signal is evidence, not
// yet a marketplace commitment.
type ListingService struct {
	signals  Repository
	listings ListingRepository
	events   event.Writer
	audits   audit.Writer
	logger   *slog.Logger
}

func NewListingService(signals Repository, listings ListingRepository, events event.Writer, audits audit.Writer, logger *slog.Logger) *ListingService {
	return &ListingService{signals: signals, listings: listings, events: events, audits: audits, logger: logger}
}

// PublishFromSignal creates (or reuses, if a duplicate is detected) a
// SupplyListing from a SupplySignal. Per PRD 3 §11: duplicate detection
// is deterministic (same vendor + normalized product + variant + price,
// not expired) — no ML. If a duplicate is found, its freshness is
// bumped (last_verified_at) rather than creating a second active listing.
func (s *ListingService) PublishFromSignal(ctx context.Context, signalID uuid.UUID, operatorID *uuid.UUID) (*Listing, error) {
	existingListing, err := s.listings.FindBySignalID(ctx, signalID)
	if err != nil {
		return nil, fmt.Errorf("checking existing listing for signal: %w", err)
	}
	if existingListing != nil {
		return existingListing, nil // already published from this exact signal — idempotent
	}

	sig, err := s.findSignal(ctx, signalID)
	if err != nil {
		return nil, err
	}
	if sig == nil {
		return nil, fmt.Errorf("supply signal %s not found", signalID)
	}

	normalized := ""
	if sig.Product != nil {
		normalized = strings.ToLower(strings.Join(strings.Fields(*sig.Product), ""))
	}
	var normalizedPtr *string
	if normalized != "" {
		normalizedPtr = &normalized
	}

	if normalized != "" {
		dup, err := s.listings.FindRecentDuplicate(ctx, sig.VendorID, normalized, sig.Variant, sig.Price)
		if err != nil {
			s.logger.Error("checking for duplicate listing", "error", err)
		} else if dup != nil {
			// Same product/variant/price already active for this vendor —
			// treat the new signal as freshness confirmation rather than
			// spawning a duplicate active listing (PRD 3 §11).
			updated, err := s.listings.Update(ctx, dup.ID, ListingFields{})
			if err != nil {
				return nil, fmt.Errorf("refreshing duplicate listing: %w", err)
			}
			if err := s.events.Write(ctx, &event.Event{
				EventType: event.SupplyListingDuplicateRefreshed, EntityType: "supply_listing", EntityID: &dup.ID,
				ActorType: event.ActorSystem, Payload: map[string]any{"signal_id": signalID.String()},
			}); err != nil {
				s.logger.Error("writing duplicate-refresh event", "error", err)
			}
			return updated, nil
		}
	}

	listing := &Listing{
		VendorID: sig.VendorID, SupplySignalID: &signalID,
		Category: sig.Category, Product: sig.Product, NormalizedProduct: normalizedPtr, Variant: sig.Variant, Condition: sig.Condition,
		Price: sig.Price, Quantity: sig.Quantity, Location: sig.Location,
		Status: ListingDraft, SourceChannel: "whatsapp",
		ExtractionMethod: "heuristic", Confidence: sig.Confidence,
		ExpiresAt: sig.ExpiresAt,
	}

	created, err := s.listings.Create(ctx, listing)
	if err != nil {
		return nil, fmt.Errorf("creating listing: %w", err)
	}

	if err := s.events.Write(ctx, &event.Event{
		EventType: event.SupplyListingCreated, EntityType: "supply_listing", EntityID: &created.ID,
		ActorType: event.ActorOperator, ActorID: operatorID, Payload: map[string]any{"signal_id": signalID.String()},
	}); err != nil {
		s.logger.Error("writing SUPPLY_LISTING_CREATED event", "error", err)
	}

	return created, nil
}

// Activate transitions a DRAFT listing to ACTIVE (visible to discovery).
func (s *ListingService) Activate(ctx context.Context, listingID uuid.UUID, operatorID *uuid.UUID) (*Listing, error) {
	active := ListingActive
	updated, err := s.listings.Update(ctx, listingID, ListingFields{Status: &active})
	if err != nil {
		return nil, err
	}
	if err := s.events.Write(ctx, &event.Event{
		EventType: event.SupplyListingUpdated, EntityType: "supply_listing", EntityID: &listingID,
		ActorType: event.ActorOperator, ActorID: operatorID, Payload: map[string]any{"status": "ACTIVE"},
	}); err != nil {
		s.logger.Error("writing SUPPLY_LISTING_UPDATED event", "error", err)
	}
	return updated, nil
}

// Correct applies an operator correction with a full before/after audit
// trail, following the same philosophy established for VendorResponse in
// Phase 2.1.
func (s *ListingService) Correct(ctx context.Context, listingID uuid.UUID, fields ListingFields, operatorID *uuid.UUID) (*Listing, error) {
	before, err := s.listings.FindByID(ctx, listingID)
	if err != nil {
		return nil, fmt.Errorf("loading listing before correction: %w", err)
	}
	if before == nil {
		return nil, fmt.Errorf("listing %s not found", listingID)
	}

	after, err := s.listings.Update(ctx, listingID, fields)
	if err != nil {
		return nil, fmt.Errorf("applying correction: %w", err)
	}

	prev, newV := diffListingFields(before, fields)
	if len(prev) == 0 {
		return after, nil
	}

	if err := s.audits.Write(ctx, &audit.Entry{
		ActorType: "OPERATOR", ActorID: operatorID, Action: "SUPPLY_LISTING_CORRECTED",
		EntityType: "supply_listing", EntityID: &listingID, PreviousValue: prev, NewValue: newV,
	}); err != nil {
		return nil, fmt.Errorf("correction applied but audit write failed: %w", err)
	}

	if err := s.events.Write(ctx, &event.Event{
		EventType: event.SupplyListingUpdated, EntityType: "supply_listing", EntityID: &listingID,
		ActorType: event.ActorOperator, ActorID: operatorID, Payload: map[string]any{"previous": prev, "new": newV},
	}); err != nil {
		s.logger.Error("writing SUPPLY_LISTING_UPDATED event", "error", err)
	}

	return after, nil
}

func (s *ListingService) findSignal(ctx context.Context, signalID uuid.UUID) (*Signal, error) {
	return s.signals.FindByID(ctx, signalID)
}

func diffListingFields(current *Listing, patch ListingFields) (previous, newVals map[string]any) {
	previous = map[string]any{}
	newVals = map[string]any{}

	if patch.Product != nil && notEqualStrPtr(current.Product, patch.Product) {
		previous["product"] = derefStrPtr(current.Product)
		newVals["product"] = derefStrPtr(patch.Product)
	}
	if patch.Variant != nil && notEqualStrPtr(current.Variant, patch.Variant) {
		previous["variant"] = derefStrPtr(current.Variant)
		newVals["variant"] = derefStrPtr(patch.Variant)
	}
	if patch.Condition != nil && notEqualStrPtr(current.Condition, patch.Condition) {
		previous["condition"] = derefStrPtr(current.Condition)
		newVals["condition"] = derefStrPtr(patch.Condition)
	}
	if patch.Price != nil && notEqualInt64Ptr(current.Price, patch.Price) {
		previous["price"] = derefInt64Ptr(current.Price)
		newVals["price"] = derefInt64Ptr(patch.Price)
	}
	if patch.Location != nil && notEqualStrPtr(current.Location, patch.Location) {
		previous["location"] = derefStrPtr(current.Location)
		newVals["location"] = derefStrPtr(patch.Location)
	}
	if patch.Status != nil && current.Status != *patch.Status {
		previous["status"] = string(current.Status)
		newVals["status"] = string(*patch.Status)
	}

	return previous, newVals
}

func notEqualStrPtr(a, b *string) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return *a != *b
}
func notEqualInt64Ptr(a, b *int64) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return *a != *b
}
func derefStrPtr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
func derefInt64Ptr(i *int64) any {
	if i == nil {
		return nil
	}
	return *i
}
