package supply

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/vendor"
)

type fakeSignalRepo struct{ signals map[uuid.UUID]*Signal }

func (f *fakeSignalRepo) Create(ctx context.Context, s *Signal) (*Signal, error) { return s, nil }
func (f *fakeSignalRepo) FindByID(ctx context.Context, id uuid.UUID) (*Signal, error) {
	return f.signals[id], nil
}
func (f *fakeSignalRepo) ListByVendor(ctx context.Context, vendorID uuid.UUID) ([]*Signal, error) {
	return nil, nil
}
func (f *fakeSignalRepo) FreshByNormalizedProduct(ctx context.Context, normalizedProduct string) ([]*Signal, error) {
	return nil, nil
}
func (f *fakeSignalRepo) FreshByVendor(ctx context.Context, vendorID uuid.UUID) ([]*Signal, error) {
	return nil, nil
}
func (f *fakeSignalRepo) Update(ctx context.Context, id uuid.UUID, fields SignalFields) (*Signal, error) {
	return nil, nil
}

type fakeListingRepo struct {
	listings   map[uuid.UUID]*Listing
	bySignalID map[uuid.UUID]uuid.UUID
}

func newFakeListingRepo() *fakeListingRepo {
	return &fakeListingRepo{listings: map[uuid.UUID]*Listing{}, bySignalID: map[uuid.UUID]uuid.UUID{}}
}
func (f *fakeListingRepo) Create(ctx context.Context, l *Listing) (*Listing, error) {
	if l.ID == uuid.Nil {
		l.ID = uuid.New()
	}
	cp := *l
	f.listings[l.ID] = &cp
	if l.SupplySignalID != nil {
		f.bySignalID[*l.SupplySignalID] = l.ID
	}
	out := *l
	return &out, nil
}
func (f *fakeListingRepo) Update(ctx context.Context, id uuid.UUID, fields ListingFields) (*Listing, error) {
	l, ok := f.listings[id]
	if !ok {
		return nil, nil
	}
	if fields.Status != nil {
		l.Status = *fields.Status
	}
	if fields.Price != nil {
		l.Price = fields.Price
	}
	if fields.Product != nil {
		l.Product = fields.Product
	}
	cp := *l
	return &cp, nil
}
func (f *fakeListingRepo) FindByID(ctx context.Context, id uuid.UUID) (*Listing, error) {
	l, ok := f.listings[id]
	if !ok {
		return nil, nil
	}
	cp := *l
	return &cp, nil
}
func (f *fakeListingRepo) FindBySignalID(ctx context.Context, signalID uuid.UUID) (*Listing, error) {
	id, ok := f.bySignalID[signalID]
	if !ok {
		return nil, nil
	}
	return f.FindByID(ctx, id)
}
func (f *fakeListingRepo) FindRecentDuplicate(ctx context.Context, vendorID uuid.UUID, normalizedProduct string, variant *string, price *int64) (*Listing, error) {
	for _, l := range f.listings {
		if l.VendorID != vendorID || l.NormalizedProduct == nil || *l.NormalizedProduct != normalizedProduct {
			continue
		}
		if l.Status == ListingExpired || l.Status == ListingArchived {
			continue
		}
		cp := *l
		return &cp, nil
	}
	return nil, nil
}
func (f *fakeListingRepo) Search(ctx context.Context, q ListingQuery) ([]*Listing, error) { return nil, nil }

// CreateFromSignal mirrors the Postgres semantics (same signal -> existing,
// Phase 3 duplicate -> freshness refresh, else create) without the locking,
// which the Postgres integration tests cover.
func (f *fakeListingRepo) CreateFromSignal(ctx context.Context, w ListingWrite) (*Listing, ListingOutcome, error) {
	if l, _ := f.FindBySignalID(ctx, *w.Listing.SupplySignalID); l != nil {
		return l, ListingExisting, nil
	}
	if dup, _ := f.FindRecentDuplicate(ctx, w.Listing.VendorID, *w.Listing.NormalizedProduct, w.Listing.Variant, w.Listing.Price); dup != nil {
		stored := f.listings[dup.ID]
		if stored.LastVerifiedAt == nil || w.Listing.LastVerifiedAt.After(*stored.LastVerifiedAt) {
			stored.LastVerifiedAt = w.Listing.LastVerifiedAt
		}
		cp := *stored
		return &cp, ListingRefreshed, nil
	}
	w.Listing.PublicReference = "L-TEST01"
	l, err := f.Create(ctx, w.Listing)
	return l, ListingCreated, err
}

type fakeVendors struct{ v map[uuid.UUID]*vendor.Vendor }

func (f fakeVendors) FindByID(ctx context.Context, id uuid.UUID) (*vendor.Vendor, error) { return f.v[id], nil }

type fakeMedia struct{ refs []MediaReference }

func (f fakeMedia) SignalMedia(ctx context.Context, id uuid.UUID) ([]MediaReference, []uuid.UUID, *uuid.UUID, error) {
	return f.refs, nil, nil, nil
}

// newSvc builds a service whose vendor (any ID used in the test) is ACTIVE.
func newSvc(signals *fakeSignalRepo, listings *fakeListingRepo, events *fakeEventsForListing, audits *fakeAuditsForListing, vendors ...*vendor.Vendor) *ListingService {
	vs := fakeVendors{v: map[uuid.UUID]*vendor.Vendor{}}
	for _, v := range vendors {
		vs.v[v.ID] = v
	}
	return NewListingService(signals, listings, vs, fakeMedia{}, events, audits, testLogger())
}

func activeVendor(id uuid.UUID) *vendor.Vendor {
	return &vendor.Vendor{ID: id, Status: vendor.StatusActive}
}

func fresh(s *Signal) *Signal {
	s.CreatedAt = time.Now().Add(-time.Minute)
	s.ExpiresAt = time.Now().Add(7 * 24 * time.Hour)
	if s.AvailabilityStatus == "" {
		s.AvailabilityStatus = AvailabilityAvailable
	}
	return s
}
func (f *fakeListingRepo) ListByVendor(ctx context.Context, vendorID uuid.UUID) ([]*Listing, error) {
	return nil, nil
}

type fakeEventsForListing struct{ events []*event.Event }

func (f *fakeEventsForListing) Write(ctx context.Context, e *event.Event) error {
	f.events = append(f.events, e)
	return nil
}

type fakeAuditsForListing struct{ entries []*audit.Entry }

func (f *fakeAuditsForListing) Write(ctx context.Context, e *audit.Entry) error {
	f.entries = append(f.entries, e)
	return nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func strp(s string) *string    { return &s }
func i64p(i int64) *int64      { return &i }

func TestPublishFromSignal_CreatesListingWithoutHallucinatingFields(t *testing.T) {
	signalRepo := &fakeSignalRepo{signals: map[uuid.UUID]*Signal{}}
	listingRepo := newFakeListingRepo()
	events := &fakeEventsForListing{}
	sigID := uuid.New()
	vendorID := uuid.New()
	svc := newSvc(signalRepo, listingRepo, events, &fakeAuditsForListing{}, activeVendor(vendorID))
	signalRepo.signals[sigID] = fresh(&Signal{
		ID: sigID, VendorID: vendorID, Product: strp("iPhone 13"), Variant: strp("128GB"),
		Price: i64p(620000), RawMessage: "iPhone 13 128gb, 620k. I get am.",
		AvailabilityStatus: AvailabilityAvailable,
	})

	listing, err := svc.PublishFromSignal(context.Background(), sigID, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *listing.Product != "iPhone 13" || *listing.Variant != "128GB" || *listing.Price != 620000 {
		t.Errorf("expected fields carried over from signal, got product=%v variant=%v price=%v", listing.Product, listing.Variant, listing.Price)
	}
	// Must not hallucinate condition/location/quantity that weren't in the signal.
	if listing.Condition != nil || listing.Location != nil {
		t.Errorf("must not invent condition/location the vendor never stated, got condition=%v location=%v", listing.Condition, listing.Location)
	}
	if listing.SupplySignalID == nil || *listing.SupplySignalID != sigID {
		t.Error("expected listing to retain provenance back to the originating signal")
	}
}

func TestPublishFromSignal_DuplicateSignalRefreshesInsteadOfDuplicating(t *testing.T) {
	signalRepo := &fakeSignalRepo{signals: map[uuid.UUID]*Signal{}}
	listingRepo := newFakeListingRepo()
	vendorID := uuid.New()
	svc := newSvc(signalRepo, listingRepo, &fakeEventsForListing{}, &fakeAuditsForListing{}, activeVendor(vendorID))
	sig1ID, sig2ID := uuid.New(), uuid.New()
	signalRepo.signals[sig1ID] = fresh(&Signal{ID: sig1ID, VendorID: vendorID, Product: strp("iPhone 13"), Price: i64p(620000)})
	signalRepo.signals[sig2ID] = fresh(&Signal{ID: sig2ID, VendorID: vendorID, Product: strp("iPhone 13"), Price: i64p(620000)})

	first, err := svc.PublishFromSignal(context.Background(), sig1ID, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	second, err := svc.PublishFromSignal(context.Background(), sig2ID, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(listingRepo.listings) != 1 {
		t.Fatalf("expected exactly 1 listing after a duplicate signal, got %d", len(listingRepo.listings))
	}
	if first.ID != second.ID {
		t.Error("expected the duplicate signal to refresh the same listing, not create a new one")
	}
}

func TestPublishFromSignal_SameSignalTwiceIsIdempotent(t *testing.T) {
	signalRepo := &fakeSignalRepo{signals: map[uuid.UUID]*Signal{}}
	listingRepo := newFakeListingRepo()
	sigID, vendorID := uuid.New(), uuid.New()
	svc := newSvc(signalRepo, listingRepo, &fakeEventsForListing{}, &fakeAuditsForListing{}, activeVendor(vendorID))
	signalRepo.signals[sigID] = fresh(&Signal{ID: sigID, VendorID: vendorID, Product: strp("PS5"), Price: i64p(780000)})

	svc.PublishFromSignal(context.Background(), sigID, nil)
	svc.PublishFromSignal(context.Background(), sigID, nil) // same signal again

	if len(listingRepo.listings) != 1 {
		t.Fatalf("expected exactly 1 listing when publishing the same signal twice, got %d", len(listingRepo.listings))
	}
}

func TestCorrect_PreservesRawProvenancePersistsCorrectionWithAudit(t *testing.T) {
	listingRepo := newFakeListingRepo()
	audits := &fakeAuditsForListing{}
	svc := newSvc(&fakeSignalRepo{}, listingRepo, &fakeEventsForListing{}, audits)

	listingID := uuid.New()
	listingRepo.listings[listingID] = &Listing{ID: listingID, Price: i64p(620000), Status: ListingDraft}

	newPrice := int64(650000)
	updated, err := svc.Correct(context.Background(), listingID, ListingFields{Price: &newPrice}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if *updated.Price != 650000 {
		t.Errorf("expected corrected price 650000, got %d", *updated.Price)
	}
	if len(audits.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audits.entries))
	}
	if audits.entries[0].PreviousValue["price"] != int64(620000) {
		t.Errorf("expected previous price 620000 in audit, got %v", audits.entries[0].PreviousValue["price"])
	}
}

func TestActivate_TransitionsStatusToActive(t *testing.T) {
	listingRepo := newFakeListingRepo()
	svc := newSvc(&fakeSignalRepo{}, listingRepo, &fakeEventsForListing{}, &fakeAuditsForListing{})

	listingID := uuid.New()
	listingRepo.listings[listingID] = &Listing{ID: listingID, Status: ListingDraft}

	updated, err := svc.Activate(context.Background(), listingID, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated.Status != ListingActive {
		t.Errorf("expected status ACTIVE, got %s", updated.Status)
	}
}
