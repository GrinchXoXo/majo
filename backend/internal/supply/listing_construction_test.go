package supply

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/vendor"
)

// Phase 4A.3 unit tests for the pure parts of SupplySignal -> SupplyListing.

func TestNormalizeSearch(t *testing.T) {
	for in, want := range map[string]string{
		"iPhone 13": "iphone13", "iphone 13": "iphone13", "iphone13": "iphone13", "IPHONE-13": "iphone13",
		"iPhone 13 Pro Max": "iphone13promax", "Benin City": "benincity", "BENIN CITY": "benincity",
		"benin city": "benincity", "Port Harcourt": "portharcourt", "  ": "", "₦": "",
	} {
		if got := NormalizeSearch(in); got != want {
			t.Fatalf("NormalizeSearch(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewPublicReference_Format(t *testing.T) {
	re := regexp.MustCompile(`^L-[0-9A-HJKMNP-TV-Z]{6}$`)
	seen := map[string]bool{}
	for i := 0; i < 2000; i++ {
		ref, err := NewPublicReference()
		if err != nil || !re.MatchString(ref) {
			t.Fatalf("bad reference %q (%v)", ref, err)
		}
		seen[ref] = true
	}
	if len(seen) < 1990 {
		t.Fatalf("references are not random enough: %d distinct of 2000", len(seen))
	}
}

func eligibleSignal() *Signal {
	return fresh(&Signal{ID: uuid.New(), VendorID: uuid.New(), Product: strp("iPhone 13"), Price: i64p(620000)})
}

func TestEligibility(t *testing.T) {
	now := time.Now()
	act := &vendor.Vendor{Status: vendor.StatusActive}
	cases := []struct {
		name string
		mod  func(s *Signal, v *vendor.Vendor) *vendor.Vendor
		ok   bool
	}{
		{"eligible", func(s *Signal, v *vendor.Vendor) *vendor.Vendor { return v }, true},
		{"can source", func(s *Signal, v *vendor.Vendor) *vendor.Vendor {
			s.AvailabilityStatus = AvailabilityCanSource
			return v
		}, true},
		{"'hello' (no product)", func(s *Signal, v *vendor.Vendor) *vendor.Vendor { s.Product = nil; s.RawMessage = "hello"; return v }, false},
		{"blank product", func(s *Signal, v *vendor.Vendor) *vendor.Vendor { s.Product = strp("  "); return v }, false},
		{"media-only incomplete", func(s *Signal, v *vendor.Vendor) *vendor.Vendor {
			s.Product = nil
			s.ExtractedData = map[string]any{"incomplete": true}
			return v
		}, false},
		{"unavailable", func(s *Signal, v *vendor.Vendor) *vendor.Vendor {
			s.AvailabilityStatus = AvailabilityUnavailable
			return v
		}, false},
		{"expired evidence", func(s *Signal, v *vendor.Vendor) *vendor.Vendor { s.ExpiresAt = now.Add(-time.Hour); return v }, false},
		{"blocked vendor", func(s *Signal, v *vendor.Vendor) *vendor.Vendor { return &vendor.Vendor{Status: vendor.StatusBlocked} }, false},
		{"inactive vendor", func(s *Signal, v *vendor.Vendor) *vendor.Vendor { return &vendor.Vendor{Status: vendor.StatusInactive} }, false},
		{"unknown vendor", func(s *Signal, v *vendor.Vendor) *vendor.Vendor { return nil }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := eligibleSignal()
			v := c.mod(s, act)
			err := eligibility(s, v, now)
			var inel ErrIneligible
			if c.ok != (err == nil) || (err != nil && !errors.As(err, &inel)) {
				t.Fatalf("eligible=%v, err=%v", c.ok, err)
			}
		})
	}
}

func TestResolveLocation(t *testing.T) {
	benin := &vendor.Vendor{PrimaryLocation: "Benin City"}
	s := eligibleSignal()
	s.Location = strp("Port Harcourt")
	if l, src := resolveLocation(s, benin); *l != "Port Harcourt" || src != LocationFromSignal {
		t.Fatalf("explicit location must win, got %v %s", *l, src)
	}
	s.Location = nil
	if l, src := resolveLocation(s, benin); *l != "Benin City" || src != LocationFromVendorPrimary {
		t.Fatalf("fallback to vendor primary location, got %v %s", l, src)
	}
	if l, src := resolveLocation(s, &vendor.Vendor{ServiceAreas: []string{"Lagos"}}); l != nil || src != LocationNone {
		t.Fatalf("no location (service areas are not a location), got %v %s", l, src)
	}
}

func TestBuildListing_MapsWithoutInventingAndKeepsMediaOrder(t *testing.T) {
	s := eligibleSignal()
	s.Category, s.Variant, s.Quantity, s.Currency = strp("electronics"), strp("128GB"), func() *int { q := 4; return &q }(), "NGN"
	media := []MediaReference{{Type: "image", AssetID: "c"}, {Type: "image", AssetID: "a"}, {Type: "video", AssetID: "b"}}
	first := uuid.New()
	l, _ := buildListing(s, &vendor.Vendor{PrimaryLocation: "Benin City"}, media, &first)
	if l.Status != ListingDraft || *l.Product != "iPhone 13" || *l.NormalizedProduct != "iphone13" || *l.Category != "electronics" ||
		*l.Variant != "128GB" || *l.Price != 620000 || l.Currency != "NGN" || *l.Quantity != 4 || l.Condition != nil ||
		*l.Location != "Benin City" || *l.SupplySignalID != s.ID || *l.SourceMessageID != first {
		t.Fatalf("mapping wrong: %+v", l)
	}
	if l.MediaReferences[0].AssetID != "c" || l.MediaReferences[1].AssetID != "a" || l.MediaReferences[2].AssetID != "b" {
		t.Fatal("media order must be the evidence order")
	}
	if !l.LastVerifiedAt.Equal(s.CreatedAt) || !l.ExpiresAt.Equal(s.CreatedAt.Add(ListingLifetime)) {
		t.Fatalf("verified = evidence time, expires = +lifetime; got %v / %v", l.LastVerifiedAt, l.ExpiresAt)
	}
	if *l.Product == *l.NormalizedProduct {
		t.Fatal("display value must not be replaced by the search form")
	}
}

func TestHandleDraftJob_IneligibleIsNotAFailure(t *testing.T) {
	signals := &fakeSignalRepo{signals: map[uuid.UUID]*Signal{}}
	s := eligibleSignal()
	s.Product = nil
	signals.signals[s.ID] = s
	listings := newFakeListingRepo()
	svc := newSvc(signals, listings, &fakeEventsForListing{}, &fakeAuditsForListing{}, activeVendor(s.VendorID))
	if err := svc.HandleDraftJob(context.Background(), &jobs.Job{Payload: map[string]any{"supply_signal_id": s.ID.String()}}); err != nil {
		t.Fatalf("ineligible signal should complete the job, got %v", err)
	}
	if len(listings.listings) != 0 {
		t.Fatal("no listing for an ineligible signal")
	}
	if err := svc.HandleDraftJob(context.Background(), &jobs.Job{Payload: map[string]any{"supply_signal_id": uuid.NewString()}}); !jobs.IsPermanent(err) {
		t.Fatalf("unknown signal is a permanent failure, got %v", err)
	}
}

func TestCreateFromSignal_IsAlwaysDraft(t *testing.T) {
	signals := &fakeSignalRepo{signals: map[uuid.UUID]*Signal{}}
	s := eligibleSignal()
	s.Confidence = 0.95 // high confidence must not publish
	signals.signals[s.ID] = s
	svc := newSvc(signals, newFakeListingRepo(), &fakeEventsForListing{}, &fakeAuditsForListing{}, activeVendor(s.VendorID))
	l, outcome, err := svc.CreateFromSignal(context.Background(), s.ID, "SYSTEM", nil)
	if err != nil || outcome != ListingCreated || l.Status != ListingDraft || l.PublishedAt != nil {
		t.Fatalf("expected a DRAFT, got %+v %s %v", l, outcome, err)
	}
}
