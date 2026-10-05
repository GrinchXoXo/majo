package supply

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/event"
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
	svc := NewListingService(signalRepo, listingRepo, events, &fakeAuditsForListing{}, testLogger())

	sigID := uuid.New()
	vendorID := uuid.New()
	signalRepo.signals[sigID] = &Signal{
		ID: sigID, VendorID: vendorID, Product: strp("iPhone 13"), Variant: strp("128GB"),
		Price: i64p(620000), RawMessage: "iPhone 13 128gb, 620k. I get am.",
		AvailabilityStatus: AvailabilityAvailable,
	}

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
	svc := NewListingService(signalRepo, listingRepo, &fakeEventsForListing{}, &fakeAuditsForListing{}, testLogger())

	vendorID := uuid.New()
	sig1ID, sig2ID := uuid.New(), uuid.New()
	signalRepo.signals[sig1ID] = &Signal{ID: sig1ID, VendorID: vendorID, Product: strp("iPhone 13"), Price: i64p(620000)}
	signalRepo.signals[sig2ID] = &Signal{ID: sig2ID, VendorID: vendorID, Product: strp("iPhone 13"), Price: i64p(620000)}

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
	svc := NewListingService(signalRepo, listingRepo, &fakeEventsForListing{}, &fakeAuditsForListing{}, testLogger())

	sigID, vendorID := uuid.New(), uuid.New()
	signalRepo.signals[sigID] = &Signal{ID: sigID, VendorID: vendorID, Product: strp("PS5"), Price: i64p(780000)}

	svc.PublishFromSignal(context.Background(), sigID, nil)
	svc.PublishFromSignal(context.Background(), sigID, nil) // same signal again

	if len(listingRepo.listings) != 1 {
		t.Fatalf("expected exactly 1 listing when publishing the same signal twice, got %d", len(listingRepo.listings))
	}
}

func TestCorrect_PreservesRawProvenancePersistsCorrectionWithAudit(t *testing.T) {
	listingRepo := newFakeListingRepo()
	audits := &fakeAuditsForListing{}
	svc := NewListingService(&fakeSignalRepo{}, listingRepo, &fakeEventsForListing{}, audits, testLogger())

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
	svc := NewListingService(&fakeSignalRepo{}, listingRepo, &fakeEventsForListing{}, &fakeAuditsForListing{}, testLogger())

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
