package supply

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
)

// ListingService turns a vendor-submitted SupplySignal into durable,
// discoverable marketplace supply (PRD 3 §5-6). Publishing is an
// explicit operator action, not automatic — a signal is evidence, not
// yet a marketplace commitment.
type ListingService struct {
	signals  Repository
	listings ListingRepository
	vendors  VendorLookup
	media    SignalMediaReader
	events   event.Writer
	audits   audit.Writer
	logger   *slog.Logger

	// Phase 4A.3 automatic DRAFT creation (optional; see SetDraftJobs).
	jobs        jobs.Store
	maxAttempts int
}

func NewListingService(signals Repository, listings ListingRepository, vendors VendorLookup, media SignalMediaReader,
	events event.Writer, audits audit.Writer, logger *slog.Logger) *ListingService {
	return &ListingService{signals: signals, listings: listings, vendors: vendors, media: media,
		events: events, audits: audits, logger: logger, maxAttempts: 1}
}

// PublishFromSignal is the Phase 3 operator entry point (POST
// /api/supply-listings). Despite its name it never publishes: it builds a
// DRAFT (or returns the existing/refreshed listing) through the canonical
// Phase 4A.3 path, CreateFromSignal. Activation stays a separate operator
// action (Activate).
func (s *ListingService) PublishFromSignal(ctx context.Context, signalID uuid.UUID, operatorID *uuid.UUID) (*Listing, error) {
	l, _, err := s.CreateFromSignal(ctx, signalID, event.ActorOperator, operatorID)
	return l, err
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
