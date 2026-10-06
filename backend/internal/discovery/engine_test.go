package discovery

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/supply"
)

var t0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

func days(n float64) time.Duration { return time.Duration(n * float64(24*time.Hour)) }

func sp(s string) *string { return &s }
func ip(n int64) *int64   { return &n }

// item is a published listing (pubAt) verified at verifiedAt.
func item(ref, product, category, location string, price int64, pubAt, verifiedAt time.Time) SupplyItem {
	p := supply.MustDefaultPolicy()
	return SupplyItem{
		Listing: &supply.Listing{
			ID: uuid.NewSHA1(uuid.NameSpaceURL, []byte(ref)), PublicReference: ref, VendorID: uuid.New(),
			Status: supply.ListingActive, Product: sp(product), Category: sp(category), Condition: sp("New"),
			Price: ip(price), Location: sp(location), CreatedAt: pubAt.Add(-time.Hour), PublishedAt: &pubAt,
			LastVerifiedAt: &verifiedAt, ExpiresAt: p.CeilingFrom(pubAt),
		},
		VendorActive: true, Images: 1,
	}
}

func run(items []SupplyItem, v ViewerContext, s Scope, now time.Time) Result {
	return NewEngine(nil).Run(items, v, s, DefaultParams(), now)
}

func refs(cs []Candidate) []string {
	out := []string{}
	for _, c := range cs {
		out = append(out, c.PublicReference)
	}
	return out
}

func excluded(r Result, ref string) *Exclusion {
	for i := range r.Exclusions {
		if r.Exclusions[i].PublicReference == ref {
			return &r.Exclusions[i]
		}
	}
	return nil
}

var benin = ViewerContext{Location: "GRA, Benin City", Interests: []string{"electronics"}}

func TestDiscovery_EligibilityComesFromTheCanonicalPolicy(t *testing.T) {
	now := t0.Add(days(5))
	ok := item("L-OK0001", "Samsung TV", "electronics", "GRA, Benin City", 400000, t0, now.Add(-time.Hour))
	expired := item("L-EXP001", "Samsung TV", "electronics", "GRA, Benin City", 400000, t0.Add(-days(61)), t0.Add(-days(61)))
	draft := item("L-DRF001", "Samsung TV", "electronics", "GRA, Benin City", 400000, t0, t0)
	draft.Listing.Status, draft.Listing.PublishedAt = supply.ListingDraft, nil
	paused := item("L-PAU001", "Samsung TV", "electronics", "GRA, Benin City", 400000, t0, t0)
	paused.Listing.Status = supply.ListingPaused
	unavailable := item("L-UNA001", "Samsung TV", "electronics", "GRA, Benin City", 400000, t0, t0)
	unavailable.Listing.Status = supply.ListingUnavailable
	rejected := item("L-REJ001", "Samsung TV", "electronics", "GRA, Benin City", 400000, t0, t0)
	rejected.Listing.Status, rejected.Listing.RejectedAt = supply.ListingArchived, &t0
	inactive := item("L-INA001", "Samsung TV", "electronics", "GRA, Benin City", 400000, t0, t0)
	inactive.VendorActive = false
	noPrice := item("L-NOP001", "Samsung TV", "electronics", "GRA, Benin City", 400000, t0, t0)
	noPrice.Listing.Price = nil

	r := run([]SupplyItem{ok, expired, draft, paused, unavailable, rejected, inactive, noPrice}, benin, Scope{Limit: 10}, now)
	if !reflect.DeepEqual(refs(r.Candidates), []string{"L-OK0001"}) {
		t.Fatalf("only the eligible listing is a candidate: %v", refs(r.Candidates))
	}
	want := map[string]string{
		"L-EXP001": "lifecycle_expired", "L-DRF001": "not_published", "L-PAU001": "status_PAUSED",
		"L-UNA001": "status_UNAVAILABLE", "L-REJ001": "rejected", "L-INA001": "vendor_inactive", "L-NOP001": "no_price",
	}
	for ref, code := range want {
		ex := excluded(r, ref)
		if ex == nil || ex.Stage != StageEligibility || ex.Code != code {
			t.Errorf("%s: want eligibility/%s, got %+v", ref, code, ex)
		}
	}
	if r.Considered != 8 {
		t.Fatalf("considered %d", r.Considered)
	}
}

func TestDiscovery_StaleFollowsTheCanonicalPolicy(t *testing.T) {
	now := t0.Add(days(35))
	stale := item("L-STA001", "Samsung TV", "electronics", "GRA, Benin City", 400000, t0, now.Add(-days(5)))
	fresh := item("L-FRE001", "Samsung TV", "electronics", "GRA, Benin City", 400000, t0, now.Add(-time.Hour))
	r := run([]SupplyItem{stale, fresh}, benin, Scope{Limit: 5}, now)
	if !reflect.DeepEqual(refs(r.Candidates), []string{"L-FRE001", "L-STA001"}) {
		t.Fatalf("stale is eligible (not expired) and ranked below fresh: %v", refs(r.Candidates))
	}
	st := r.Candidates[1]
	if !st.NeedsReconfirmation() || st.Eligibility.Freshness.State != supply.Stale || st.Eligibility.Lifecycle.Expired {
		t.Fatalf("stale candidate must carry the reconfirmation note: %+v", st.Notes)
	}
	p := DefaultParams()
	p.ExcludeStale = true
	r2 := NewEngine(nil).Run([]SupplyItem{stale, fresh}, benin, Scope{Limit: 5}, p, now)
	if ex := excluded(r2, "L-STA001"); ex == nil || ex.Code != CodeStaleExcluded {
		t.Fatalf("explicit stale exclusion: %+v", r2.Exclusions)
	}
}

// The geographic stages come from the canonical policy: a buyer elsewhere
// in the city, the state and the region becomes eligible exactly at the
// configured boundary; nobody after the ceiling.
func TestDiscovery_GeographicExpansionUsesTheCanonicalPolicy(t *testing.T) {
	cfg := supply.MustDefaultPolicy().Config()
	l := item("L-GEO001", "Samsung TV", "electronics", "GRA, Benin City", 400000, t0, t0)
	viewers := map[string]string{"local": "GRA, Benin City", "nearby": "Ugbowo, Benin City", "state": "Edo", "region": "Warri", "outside": "Lagos"}
	at := []struct {
		d       time.Duration
		visible []string
	}{
		{days(1), []string{"local"}},
		{cfg.GeoNearbyAfter - time.Second, []string{"local"}},
		{cfg.GeoNearbyAfter, []string{"local", "nearby"}},
		{cfg.GeoStateAfter, []string{"local", "nearby", "state"}},
		{cfg.GeoRegionAfter, []string{"local", "nearby", "state", "region"}},
		{cfg.MaxLifecycle, []string{}},
	}
	for _, c := range at {
		now := t0.Add(c.d)
		l.Listing.LastVerifiedAt = &now // keep fresh: freshness is not lifecycle
		got := []string{}
		for _, name := range []string{"local", "nearby", "state", "region", "outside"} {
			r := run([]SupplyItem{l}, ViewerContext{Location: viewers[name]}, Scope{}, now)
			if len(r.Candidates) == 1 {
				got = append(got, name)
			} else if ex := excluded(r, "L-GEO001"); ex == nil || (ex.Code != CodeGeoOutOfScope && ex.Code != "lifecycle_expired") {
				t.Fatalf("%s at %v: wrong exclusion %+v", name, c.d, ex)
			}
		}
		if !reflect.DeepEqual(got, c.visible) {
			t.Errorf("at %.1f days: visible to %v, want %v", c.d.Hours()/24, got, c.visible)
		}
	}
	// The reason names the expansion, for diagnostics and ranking.
	now := t0.Add(cfg.GeoNearbyAfter + days(1))
	r := run([]SupplyItem{l}, ViewerContext{Location: viewers["nearby"]}, Scope{}, now)
	if len(r.Candidates) != 1 || !hasReason(r.Candidates[0], "location", "local search window elapsed") {
		t.Fatalf("nearby reason: %+v", r.Candidates)
	}
	if r := run([]SupplyItem{l}, ViewerContext{}, Scope{}, now); excluded(r, "L-GEO001").Code != CodeGeoUnknown {
		t.Fatalf("unknown viewer location: %+v", r.Exclusions)
	}
}

func TestDiscovery_LocalRanksAboveNearby(t *testing.T) {
	now := t0.Add(days(25))
	local := item("L-LOC001", "Samsung TV", "electronics", "GRA, Benin City", 400000, t0, now)
	nearby := item("L-NEA001", "Samsung TV", "electronics", "Ugbowo, Benin City", 400000, t0, now)
	r := run([]SupplyItem{nearby, local}, benin, Scope{}, now)
	if !reflect.DeepEqual(refs(r.Candidates), []string{"L-LOC001", "L-NEA001"}) {
		t.Fatalf("local first: %v", refs(r.Candidates))
	}
	if r.Candidates[0].Score-r.Candidates[1].Score != DefaultWeights().LocationLocal-DefaultWeights().LocationNearby {
		t.Fatalf("location weights: %d vs %d", r.Candidates[0].Score, r.Candidates[1].Score)
	}
}

func TestDiscovery_ScoringDimensions(t *testing.T) {
	now := t0.Add(days(1))
	base := item("L-BAS001", "Samsung TV 55", "electronics", "GRA, Benin City", 400000, t0, now)
	w := DefaultWeights()
	score := func(it SupplyItem, v ViewerContext, s Scope) Candidate {
		r := run([]SupplyItem{it}, v, s, now)
		if len(r.Candidates) != 1 {
			t.Fatalf("no candidate: %+v", r.Exclusions)
		}
		return r.Candidates[0]
	}
	plain := score(base, ViewerContext{Location: "GRA, Benin City"}, Scope{})
	// category
	if c := score(base, benin, Scope{}); c.Score-plain.Score != w.Category || !hasReason(c, "category", "electronics") {
		t.Fatalf("category: %d vs %d %+v", c.Score, plain.Score, c.Reasons)
	}
	// family category ("phones" is in the electronics family, as in matching)
	if c := score(base, ViewerContext{Location: "GRA, Benin City", Interests: []string{"phones"}}, Scope{}); !hasReason(c, "category", "phones") {
		t.Fatalf("category family: %+v", c.Reasons)
	}
	// product (exact / related)
	if c := score(base, ViewerContext{Location: "GRA, Benin City", RecentProducts: []string{"samsung tv 55"}}, Scope{}); c.Score-plain.Score != w.Product {
		t.Fatalf("exact product: %+v", c.Reasons)
	}
	if c := score(base, ViewerContext{Location: "GRA, Benin City", RecentProducts: []string{"samsung"}}, Scope{}); c.Score-plain.Score != w.Product/2 {
		t.Fatalf("related product: %+v", c.Reasons)
	}
	// budget: within adds, over subtracts, never excludes
	if c := score(base, ViewerContext{Location: "GRA, Benin City", BudgetMax: ip(500000)}, Scope{}); c.Score-plain.Score != w.Budget {
		t.Fatalf("within budget: %+v", c.Reasons)
	}
	if c := score(base, ViewerContext{Location: "GRA, Benin City", BudgetMax: ip(100000)}, Scope{}); c.Score-plain.Score != w.OverBudget {
		t.Fatalf("over budget: %+v", c.Reasons)
	}
	// media: no image loses the media points; video adds
	noMedia := base
	noMedia.Images = 0
	if c := score(noMedia, ViewerContext{Location: "GRA, Benin City"}, Scope{}); plain.Score-c.Score != w.Media {
		t.Fatalf("media: %+v", c.Reasons)
	}
	video := base
	video.HasVideo = true
	if c := score(video, ViewerContext{Location: "GRA, Benin City"}, Scope{}); c.Score-plain.Score != w.Video || !c.HasVideo {
		t.Fatalf("video: %+v", c.Reasons)
	}
	// exposure: down-ranked, not excluded
	if c := score(base, ViewerContext{Location: "GRA, Benin City", RecentlyShown: map[uuid.UUID]bool{base.Listing.ID: true}}, Scope{}); c.Score-plain.Score != w.Exposure {
		t.Fatalf("exposure: %+v", c.Reasons)
	}
	// every point is explained
	sum := 0
	for _, r := range plain.Reasons {
		sum += r.Points
	}
	if sum != plain.Score {
		t.Fatalf("score %d != sum of reasons %d", plain.Score, sum)
	}
}

func TestDiscovery_RetrievalAndFilters(t *testing.T) {
	now := t0.Add(days(1))
	tv := item("L-TV0001", "Samsung TV", "electronics", "GRA, Benin City", 400000, t0, now)
	bag := item("L-BAG001", "Ladies bag", "fashion", "GRA, Benin City", 18500, t0, now)
	r := run([]SupplyItem{tv, bag}, benin, Scope{Category: "phones"}, now)
	if !reflect.DeepEqual(refs(r.Candidates), []string{"L-TV0001"}) || excluded(r, "L-BAG001").Code != CodeCategory {
		t.Fatalf("category scope: %v %+v", refs(r.Candidates), r.Exclusions)
	}
	r = run([]SupplyItem{tv, bag}, benin, Scope{PriceMax: ip(20000)}, now)
	if !reflect.DeepEqual(refs(r.Candidates), []string{"L-BAG001"}) || excluded(r, "L-TV0001").Code != CodePriceRange {
		t.Fatalf("price range: %v", refs(r.Candidates))
	}
	r = run([]SupplyItem{tv, bag}, benin, Scope{Query: "bag"}, now)
	if !reflect.DeepEqual(refs(r.Candidates), []string{"L-BAG001"}) {
		t.Fatalf("query: %v", refs(r.Candidates))
	}
	r = run([]SupplyItem{tv, bag}, ViewerContext{Location: "GRA, Benin City", NegativeCategories: []string{"fashion"}}, Scope{}, now)
	if !reflect.DeepEqual(refs(r.Candidates), []string{"L-TV0001"}) || excluded(r, "L-BAG001").Code != CodeNegative {
		t.Fatalf("negative interest: %v", refs(r.Candidates))
	}
	r = run([]SupplyItem{tv, bag}, benin, Scope{Limit: 1}, now)
	if len(r.Candidates) != 1 || excluded(r, "L-BAG001").Stage != StageLimit {
		t.Fatalf("limit: %+v", r)
	}
}

func TestDiscovery_DeterministicOrderAndReasons(t *testing.T) {
	now := t0.Add(days(2))
	var items []SupplyItem
	for _, ref := range []string{"L-C00003", "L-A00001", "L-B00002", "L-D00004"} {
		items = append(items, item(ref, "Samsung TV", "electronics", "GRA, Benin City", 400000, t0, now)) // identical scores
	}
	first := run(items, benin, Scope{Limit: 10}, now)
	if !reflect.DeepEqual(refs(first.Candidates), []string{"L-A00001", "L-B00002", "L-C00003", "L-D00004"}) {
		t.Fatalf("ties broken by reference: %v", refs(first.Candidates))
	}
	// Any input order, any number of runs: identical output.
	for i := 0; i < 20; i++ {
		shuffled := []SupplyItem{items[(i+1)%4], items[(i+3)%4], items[i%4], items[(i+2)%4]}
		again := run(shuffled, benin, Scope{Limit: 10}, now)
		if !reflect.DeepEqual(first.Candidates, again.Candidates) || first.RankingVersion != again.RankingVersion {
			t.Fatalf("run %d differs", i)
		}
	}
	if !strings.HasPrefix(first.RankingVersion, "discovery-det-v1+") {
		t.Fatalf("version %q", first.RankingVersion)
	}
	p := DefaultParams()
	p.Weights.Category = 31
	if p.Version(supply.DefaultPolicyConfig()) == first.RankingVersion {
		t.Fatal("a weight change must change the ranking version")
	}
}

func TestParseWeights(t *testing.T) {
	w, err := ParseWeights("category=40, exposure=-20", DefaultWeights())
	if err != nil || w.Category != 40 || w.Exposure != -20 || w.Product != DefaultWeights().Product {
		t.Fatalf("%+v %v", w, err)
	}
	for _, bad := range []string{"category", "nope=1", "category=x", "category=500"} {
		if _, err := ParseWeights(bad, DefaultWeights()); err == nil {
			t.Fatalf("%q must fail", bad)
		}
	}
}

func hasReason(c Candidate, dim, contains string) bool {
	for _, r := range c.Reasons {
		if r.Dimension == dim && strings.Contains(r.Detail, contains) {
			return true
		}
	}
	return false
}
