package matching

import (
	"testing"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/vendor"
)

// Phase 4A (Lock V2 §M-4A.6): ACTIVE SupplyListings are supply evidence
// for vendor relevance, alongside fresh signals. Weights are unchanged.

func phoneVendor(name string) *vendor.Vendor {
	return &vendor.Vendor{ID: uuid.New(), BusinessName: name, Status: vendor.StatusActive,
		Categories: []string{"phones"}, PrimaryLocation: "Benin City"}
}

func activeListing(v *vendor.Vendor, product, variant string, price int64) *supply.Listing {
	n := supply.NormalizeSearch(product)
	l := &supply.Listing{ID: uuid.New(), VendorID: v.ID, PublicReference: "L-ABC123", Product: &product,
		NormalizedProduct: &n, Price: &price, Currency: "NGN", Status: supply.ListingActive}
	if variant != "" {
		l.Variant = &variant
	}
	return l
}

func iphoneRequest() *request.BuyerRequest {
	return &request.BuyerRequest{Category: strPtr("electronics"), Product: strPtr("iPhone 13"), Variant: strPtr("128GB"),
		BudgetMax: i64Ptr(610000), Location: strPtr("Benin City")}
}

func TestMatch_ActiveListingIsSupplyEvidence(t *testing.T) {
	withListing, without := phoneVendor("Listed"), phoneVendor("Unlisted")
	// An operator-corrected listing: display product "Apple iPhone 13 128GB",
	// source signal "iphone 13".
	l := activeListing(withListing, "Apple iPhone 13 128GB", "128GB", 600000)
	cs := Match(iphoneRequest(), []*vendor.Vendor{without, withListing}, nil,
		map[string][]supply.MatchableListing{withListing.ID.String(): {{Listing: l, SourceProduct: supply.NormalizeSearch("iphone 13")}}},
		DefaultWeights(), DefaultMinRelevanceScore, DefaultMaxCandidates)
	if len(cs) != 2 || cs[0].Vendor.ID != withListing.ID {
		t.Fatalf("the vendor with a published listing must rank first: %+v", cs)
	}
	top := cs[0]
	if top.RelevantListing != l || top.RelevantSupply != nil || top.MatchType != MatchSupplyConfirmed {
		t.Fatalf("listing evidence not recorded: %+v", top)
	}
	if top.Breakdown["product"] != 30 || top.Breakdown["availability"] != 10 {
		t.Fatalf("exact product + variant on a published listing = full product and availability points: %v", top.Breakdown)
	}
	if !contains(top.Reasons, "SUPPLY_CONFIRMED_active_listing") || !contains(top.Reasons, "active_listing") {
		t.Fatalf("reasons must say the evidence is a listing: %v", top.Reasons)
	}
	if top.WithinBudget == nil || !*top.WithinBudget {
		t.Fatal("budget is judged on the listing's (curated) price")
	}
}

func TestMatch_CompactProductFormMatches(t *testing.T) {
	v := phoneVendor("V")
	l := activeListing(v, "IPHONE-13", "", 600000) // normalized "iphone13"
	cs := Match(iphoneRequest(), []*vendor.Vendor{v}, nil, map[string][]supply.MatchableListing{v.ID.String(): {{Listing: l}}},
		DefaultWeights(), 0, DefaultMaxCandidates)
	if len(cs) != 1 || cs[0].RelevantListing == nil || cs[0].Breakdown["product"] != 27 {
		t.Fatalf("'iPhone 13' must match 'IPHONE-13' exactly (no variant = 0.9): %+v", cs)
	}
}

func TestMatch_ListingWinsTiesSignalWinsWhenStronger(t *testing.T) {
	v := phoneVendor("V")
	sigPrice := int64(620000)
	sig := &supply.Signal{ID: uuid.New(), VendorID: v.ID, Product: strPtr("iPhone 13"), NormalizedProduct: strPtr("iphone13"),
		Variant: strPtr("128GB"), Price: &sigPrice, AvailabilityStatus: supply.AvailabilityAvailable}

	// Tie (both exact + variant): the curated listing wins, and its price decides the budget.
	l := activeListing(v, "iPhone 13", "128GB", 600000)
	cs := Match(iphoneRequest(), []*vendor.Vendor{v}, map[string][]*supply.Signal{v.ID.String(): {sig}},
		map[string][]supply.MatchableListing{v.ID.String(): {{Listing: l}}}, DefaultWeights(), 0, DefaultMaxCandidates)
	if cs[0].RelevantListing != l || cs[0].RelevantSupply != nil || !*cs[0].WithinBudget {
		t.Fatalf("listing must win a tie: %+v", cs[0])
	}

	// A weaker (family) listing loses to an exact signal.
	weak := activeListing(v, "iPhone 13 Pro Max case", "", 5000)
	cs = Match(iphoneRequest(), []*vendor.Vendor{v}, map[string][]*supply.Signal{v.ID.String(): {sig}},
		map[string][]supply.MatchableListing{v.ID.String(): {{Listing: weak}}}, DefaultWeights(), 0, DefaultMaxCandidates)
	if cs[0].RelevantSupply != sig || cs[0].RelevantListing != nil || cs[0].Breakdown["product"] != 30 {
		t.Fatalf("an exact signal beats a family listing: %+v", cs[0])
	}
	if *cs[0].WithinBudget {
		t.Fatal("budget then comes from the signal's price (620000 > 610000)")
	}
}

func TestMatch_OnlyActiveListingsCount(t *testing.T) {
	v := phoneVendor("V")
	for _, st := range []supply.ListingStatus{supply.ListingDraft, supply.ListingArchived, supply.ListingPaused, supply.ListingSold} {
		l := activeListing(v, "iPhone 13", "128GB", 600000)
		l.Status = st
		cs := Match(iphoneRequest(), []*vendor.Vendor{v}, nil, map[string][]supply.MatchableListing{v.ID.String(): {{Listing: l}}},
			DefaultWeights(), 0, DefaultMaxCandidates)
		if len(cs) != 1 || cs[0].RelevantListing != nil || cs[0].Breakdown["product"] != 0 {
			t.Fatalf("%s listing must not be evidence: %+v", st, cs)
		}
	}
}

func TestMatch_ListingOfInactiveVendorIgnored(t *testing.T) {
	v := phoneVendor("V")
	v.Status = vendor.StatusBlocked
	l := activeListing(v, "iPhone 13", "128GB", 600000)
	if cs := Match(iphoneRequest(), []*vendor.Vendor{v}, nil, map[string][]supply.MatchableListing{v.ID.String(): {{Listing: l}}},
		DefaultWeights(), 0, DefaultMaxCandidates); len(cs) != 0 {
		t.Fatal("vendor eligibility still applies to listing evidence")
	}
}

func TestMatch_WithListingsIsDeterministic(t *testing.T) {
	a := &vendor.Vendor{ID: uuid.MustParse("00000000-0000-0000-0000-00000000000a"), Status: vendor.StatusActive, Categories: []string{"phones"}}
	b := &vendor.Vendor{ID: uuid.MustParse("00000000-0000-0000-0000-00000000000b"), Status: vendor.StatusActive, Categories: []string{"phones"}}
	ls := map[string][]supply.MatchableListing{a.ID.String(): {{Listing: activeListing(a, "iPhone 13", "", 1)}}, b.ID.String(): {{Listing: activeListing(b, "iPhone 13", "", 1)}}}
	r1 := Match(iphoneRequest(), []*vendor.Vendor{b, a}, nil, ls, DefaultWeights(), 0, DefaultMaxCandidates)
	r2 := Match(iphoneRequest(), []*vendor.Vendor{a, b}, nil, ls, DefaultWeights(), 0, DefaultMaxCandidates)
	if r1[0].Vendor.ID != r2[0].Vendor.ID || r1[0].Vendor.ID != a.ID {
		t.Fatal("ranking must not depend on input order")
	}
}

func contains(xs []string, x string) bool {
	for _, s := range xs {
		if s == x {
			return true
		}
	}
	return false
}

func TestMatch_CorrectedListingWithoutSourceIsFamilyMatch(t *testing.T) {
	v := phoneVendor("V")
	l := activeListing(v, "Apple iPhone 13 128GB", "128GB", 600000)
	cs := Match(iphoneRequest(), []*vendor.Vendor{v}, nil, map[string][]supply.MatchableListing{v.ID.String(): {{Listing: l}}},
		DefaultWeights(), 0, DefaultMaxCandidates)
	if cs[0].Breakdown["product"] != 15 || cs[0].MatchType != MatchProductCapable {
		t.Fatalf("display product alone is a family match: %v", cs[0].Breakdown)
	}
}

// Publication curates a CAN_SOURCE submission; it does not upgrade it.
func TestMatch_CanSourceListingStaysCanSource(t *testing.T) {
	v := phoneVendor("V")
	l := activeListing(v, "iPhone 13", "128GB", 600000)
	cs := Match(iphoneRequest(), []*vendor.Vendor{v}, nil,
		map[string][]supply.MatchableListing{v.ID.String(): {{Listing: l, SourceAvailability: supply.AvailabilityCanSource}}},
		DefaultWeights(), 0, DefaultMaxCandidates)
	c := cs[0]
	if c.MatchType != MatchCanSource || c.Breakdown["product"] != 20 || c.Breakdown["availability"] != 6 ||
		!contains(c.Reasons, "can_source_listing") || c.RelevantListing != l {
		t.Fatalf("a published can-source listing scores as can-source: %s %v %v", c.MatchType, c.Breakdown, c.Reasons)
	}
}

// A correction to a different product drops the old (source) name.
func TestMatch_CorrectionToAnotherProductDropsSourceName(t *testing.T) {
	v := phoneVendor("V")
	l := activeListing(v, "iPhone 14", "128GB", 600000) // operator: "iphone 13" was wrong
	cs := Match(iphoneRequest(), []*vendor.Vendor{v}, nil,
		map[string][]supply.MatchableListing{v.ID.String(): {{Listing: l, SourceProduct: "iphone13"}}},
		DefaultWeights(), 0, DefaultMaxCandidates)
	if cs[0].RelevantListing != nil || cs[0].Breakdown["product"] != 0 {
		t.Fatalf("an iPhone 14 listing must not match an iPhone 13 request through its old name: %v", cs[0].Breakdown)
	}
}
