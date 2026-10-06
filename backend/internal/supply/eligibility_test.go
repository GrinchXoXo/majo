package supply

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/geo"
)

// Phase 4A.5: the canonical eligibility policy. Pure tests: fixed clock,
// no database.

var pubT0 = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC) // publication instant

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// published returns facts for an ACTIVE listing published at pubT0, with the
// default 60-day ceiling persisted, verified at `verified`.
func published(verified time.Time) ListingFacts {
	pub := pubT0
	return ListingFacts{
		ListingID: uuid.New(), Status: ListingActive, CreatedAt: pubT0.Add(-time.Hour), PublishedAt: &pub,
		LastVerifiedAt: &verified, ExpiresAt: MustDefaultPolicy().CeilingFrom(pubT0),
		Product: strp("Samsung TV 55"), Category: strp("electronics"), Price: i64p(450000),
		Location: strp("GRA, Benin City"), VendorID: uuid.New(),
	}
}

func TestEligibility_LifecycleAndGeographyBoundaries(t *testing.T) {
	p := MustDefaultPolicy()
	cases := []struct {
		day      int
		stage    GeoStage
		eligible bool
		next     GeoStage
		nextDay  int
	}{
		{0, GeoLocal, true, GeoNearby, 20},
		{19, GeoLocal, true, GeoNearby, 20},
		{20, GeoNearby, true, GeoState, 30},
		{29, GeoNearby, true, GeoState, 30},
		{30, GeoState, true, GeoRegion, 45},
		{44, GeoState, true, GeoRegion, 45},
		{45, GeoRegion, true, GeoExpired, 60},
		{59, GeoRegion, true, GeoExpired, 60},
		{60, GeoExpired, false, "", 0},
		{61, GeoExpired, false, "", 0},
	}
	for _, c := range cases {
		now := pubT0.Add(days(c.day))
		e := p.Evaluate(published(now), now) // verified "today": freshness never decides here
		if e.Geography.Stage != c.stage || e.Eligible != c.eligible || e.Lifecycle.Day != c.day {
			t.Fatalf("day %d: stage=%s eligible=%v lifecycle day=%d reasons=%v", c.day, e.Geography.Stage, e.Eligible, e.Lifecycle.Day, e.Reasons)
		}
		if c.eligible {
			if e.Lifecycle.State != LifecycleLive || e.Geography.NextStage != c.next || !e.Geography.NextExpansionAt.Equal(pubT0.Add(days(c.nextDay))) {
				t.Fatalf("day %d: next = %s at %v, want %s at day %d", c.day, e.Geography.NextStage, e.Geography.NextExpansionAt, c.next, c.nextDay)
			}
		} else if e.Lifecycle.State != LifecycleExpired || !contains(e.Reasons, "lifecycle_expired") || len(e.Geography.Levels) != 0 {
			t.Fatalf("day %d: must be expired with no geographic scope: %+v", c.day, e)
		}
	}
	// The exact boundary: eligible until the last nanosecond before day 60.
	if !p.Evaluate(published(pubT0), pubT0.Add(days(60)-time.Nanosecond)).Eligible {
		t.Fatal("eligible right up to the ceiling")
	}
	if p.Evaluate(published(pubT0), pubT0.Add(days(60))).Eligible {
		t.Fatal("not eligible at exactly the ceiling (age >= 60 days)")
	}
}

func TestEligibility_GeographicLevelsWidenCumulatively(t *testing.T) {
	p := MustDefaultPolicy()
	want := map[int][]geo.Level{
		0:  {geo.LevelExactArea, geo.LevelCommunity, geo.LevelLGA},
		20: {geo.LevelExactArea, geo.LevelCommunity, geo.LevelLGA, geo.LevelCity},
		30: {geo.LevelExactArea, geo.LevelCommunity, geo.LevelLGA, geo.LevelCity, geo.LevelState},
		45: {geo.LevelExactArea, geo.LevelCommunity, geo.LevelLGA, geo.LevelCity, geo.LevelState, geo.LevelRegion},
	}
	scopes := map[int]string{0: "ORIGIN_AREA", 20: "NEARBY_AREAS", 30: "STATE", 45: "REGION"}
	for d, levels := range want {
		now := pubT0.Add(days(d))
		g := p.Evaluate(published(now), now).Geography
		if !reflect.DeepEqual(g.Levels, levels) || g.Scope != scopes[d] {
			t.Fatalf("day %d: %s %v", d, g.Scope, g.Levels)
		}
		if g.Origin.City != "Benin City" || g.Origin.State != "Edo" || g.Origin.Region != "South-South" ||
			g.Origin.Community != "GRA" || !g.Origin.Resolved {
			t.Fatalf("origin: %+v", g.Origin)
		}
	}
}

func TestEligibility_FreshnessMatrix(t *testing.T) {
	p := MustDefaultPolicy()
	cases := []struct {
		name           string
		createdDaysAgo int
		verifiedAgo    time.Duration
		state          FreshnessState
	}{
		{"created today, verified today", 0, 0, Fresh},
		{"created 30d, verified today", 30, 0, Fresh},
		{"created 30d, verified 5d ago", 30, days(5), Stale},
		{"created 30d, verified 73h ago", 30, 73 * time.Hour, Stale},
		{"created 30d, verified 100h ago", 30, 100 * time.Hour, Stale},
		{"created 30d, verified 71h59m ago", 30, 72*time.Hour - time.Minute, Fresh},
		{"created 30d, verified exactly 72h ago", 30, 72 * time.Hour, Stale},
	}
	for _, c := range cases {
		now := pubT0.Add(days(c.createdDaysAgo))
		e := p.Evaluate(published(now.Add(-c.verifiedAgo)), now)
		if e.Freshness.State != c.state {
			t.Fatalf("%s: %s (age %s)", c.name, e.Freshness.State, e.Freshness.Age)
		}
		// Freshness never decides eligibility: all of these are inside the lifecycle.
		if !e.Eligible {
			t.Fatalf("%s: stale is not expired; must stay eligible: %v", c.name, e.Reasons)
		}
		if (c.state == Stale) != contains(e.Notes, "stale_needs_reconfirmation") {
			t.Fatalf("%s: notes %v", c.name, e.Notes)
		}
	}
}

func TestEligibility_FreshnessIsNotExpiry(t *testing.T) {
	p := MustDefaultPolicy()
	// Listing age 35 days, verified 2 days ago (48h < 72h) -> fresh, active, eligible.
	now := pubT0.Add(days(35))
	e := p.Evaluate(published(now.Add(-48*time.Hour)), now)
	if e.Freshness.State != Fresh || !e.Eligible || e.Geography.Stage != GeoState {
		t.Fatalf("35d old, verified 2d ago: %+v", e)
	}
	// Listing age 5 days, verified 5 days ago -> stale, active, eligible.
	now = pubT0.Add(days(5))
	e = p.Evaluate(published(pubT0), now)
	if e.Freshness.State != Stale || !e.Eligible || e.Lifecycle.State != LifecycleLive {
		t.Fatalf("5d old, verified 5d ago: %+v", e)
	}
	// Listing age 61 days, verified yesterday -> expired. Verification does not revive.
	now = pubT0.Add(days(61))
	e = p.Evaluate(published(now.Add(-days(1))), now)
	if e.Freshness.State != Fresh || e.Eligible || e.Lifecycle.State != LifecycleExpired || e.Geography.Stage != GeoExpired {
		t.Fatalf("61d old, verified yesterday must be expired (fresh but not eligible): %+v", e)
	}
}

func TestEligibility_StatusesAndVendor(t *testing.T) {
	p := MustDefaultPolicy()
	now := pubT0.Add(days(3))
	for st, reason := range map[ListingStatus]string{
		ListingDraft: "not_published", ListingPaused: "status_PAUSED", ListingSold: "status_SOLD",
		ListingUnavailable: "status_UNAVAILABLE", ListingArchived: "status_ARCHIVED", ListingExpired: "status_EXPIRED",
	} {
		f := published(now)
		f.Status = st
		if st == ListingDraft {
			f.PublishedAt = nil
		}
		e := p.Evaluate(f, now)
		if e.Eligible || !contains(e.Reasons, reason) {
			t.Fatalf("%s: eligible=%v reasons=%v", st, e.Eligible, e.Reasons)
		}
	}
	f := published(now)
	f.Status, f.Rejected = ListingArchived, true
	if e := p.Evaluate(f, now); e.Eligible || !contains(e.Reasons, "rejected") || e.Lifecycle.State != LifecycleClosed {
		t.Fatalf("rejected: %+v", e)
	}
	inactive := false
	f = published(now)
	f.VendorActive = &inactive
	if e := p.Evaluate(f, now); e.Eligible || !contains(e.Reasons, "vendor_inactive") {
		t.Fatalf("vendor inactive: %v", e.Reasons)
	}
	f = published(now)
	f.Price = nil
	if e := p.Evaluate(f, now); e.Eligible || !contains(e.Reasons, "no_price") {
		t.Fatalf("no price: %v", e.Reasons)
	}
	// A draft has no lifecycle yet and no geographic scope.
	f = published(now)
	f.Status, f.PublishedAt = ListingDraft, nil
	if e := p.Evaluate(f, now); e.Lifecycle.State != LifecycleNotPublished || e.Geography.Stage != GeoNone {
		t.Fatalf("draft: %+v", e)
	}
}

// The persisted ceiling is authoritative: a listing published under a
// shorter policy keeps its ceiling even if the configuration later grows,
// and the reverse.
func TestEligibility_PersistedCeilingIsAuthoritative(t *testing.T) {
	p := MustDefaultPolicy()
	f := published(pubT0)
	f.ExpiresAt = pubT0.Add(days(40)) // published when the max lifecycle was 40 days
	if e := p.Evaluate(f, pubT0.Add(days(41))); e.Eligible || e.Lifecycle.MaxDays != 40 {
		t.Fatalf("a 40-day ceiling must not be stretched by today's 60-day policy: %+v", e.Lifecycle)
	}
	// Status still says ACTIVE, the policy says expired: the policy wins.
	if e := p.Evaluate(f, pubT0.Add(days(41))); f.Status != ListingActive || e.Lifecycle.State != LifecycleExpired {
		t.Fatal("logically expired despite ACTIVE status")
	}
}

func TestEligibility_Deterministic(t *testing.T) {
	p := MustDefaultPolicy()
	f := published(pubT0.Add(days(10)))
	now := pubT0.Add(days(22) + 3*time.Hour)
	first := p.Evaluate(f, now)
	for i := 0; i < 100; i++ {
		if !reflect.DeepEqual(first, p.Evaluate(f, now)) {
			t.Fatal("same listing + same configuration + same instant must give the same result")
		}
	}
	q := MustDefaultPolicy()
	if !reflect.DeepEqual(first, q.Evaluate(f, now)) {
		t.Fatal("two policies with the same configuration must agree")
	}
}

func TestEligibility_ConfigValidation(t *testing.T) {
	bad := map[string]func(c *PolicyConfig){
		"nearby < local":         func(c *PolicyConfig) { c.GeoLocalAfter, c.GeoNearbyAfter = days(5), days(4) },
		"state < nearby":         func(c *PolicyConfig) { c.GeoStateAfter = days(19) },
		"region < state":         func(c *PolicyConfig) { c.GeoRegionAfter = days(29) },
		"max lifecycle < region": func(c *PolicyConfig) { c.MaxLifecycle = days(44) },
		"max lifecycle zero":     func(c *PolicyConfig) { c.MaxLifecycle = 0 },
		"freshness window zero":  func(c *PolicyConfig) { c.FreshnessWindow = 0 },
		"negative local":         func(c *PolicyConfig) { c.GeoLocalAfter = -time.Hour },
	}
	for name, mutate := range bad {
		c := DefaultPolicyConfig()
		mutate(&c)
		if _, err := NewEligibilityPolicy(c, nil); err == nil || !strings.Contains(err.Error(), "supply lifecycle") {
			t.Fatalf("%s: want a clear configuration error, got %v", name, err)
		}
	}
	// Equal starts are valid: the earlier stage is skipped.
	c := DefaultPolicyConfig()
	c.GeoStateAfter, c.GeoRegionAfter = days(20), days(20)
	p, err := NewEligibilityPolicy(c, nil)
	if err != nil {
		t.Fatal(err)
	}
	now := pubT0.Add(days(20))
	if g := p.Evaluate(published(now), now).Geography; g.Stage != GeoRegion || g.NextStage != GeoExpired {
		t.Fatalf("equal starts skip to the widest: %+v", g)
	}
	// A positive local start leaves new listings PENDING (not searchable).
	c = DefaultPolicyConfig()
	c.GeoLocalAfter = days(1)
	p, _ = NewEligibilityPolicy(c, nil)
	e := p.Evaluate(published(pubT0), pubT0.Add(time.Hour))
	if e.Geography.Stage != GeoPending || e.Eligible || e.Geography.NextStage != GeoLocal {
		t.Fatalf("pending local stage: %+v", e.Geography)
	}
	// max == region: the region stage never starts before the ceiling.
	c = DefaultPolicyConfig()
	c.MaxLifecycle = days(45)
	p, _ = NewEligibilityPolicy(c, nil)
	f := published(pubT0)
	f.ExpiresAt = p.CeilingFrom(pubT0)
	if g := p.Evaluate(f, pubT0.Add(days(31))).Geography; g.NextStage != GeoExpired {
		t.Fatalf("region starts at the ceiling, so the next change is expiry: %+v", g)
	}
}

func TestEligibility_UnresolvedOriginIsNotBlocking(t *testing.T) {
	p := MustDefaultPolicy()
	f := published(pubT0)
	f.Location = strp("Ogbomoso market")
	e := p.Evaluate(f, pubT0)
	if !e.Eligible || e.Geography.Origin.Resolved || !contains(e.Notes, "origin_unresolved") ||
		!strings.Contains(e.Geography.Reason, "not resolved") {
		t.Fatalf("an unknown place is still eligible, flagged: %+v", e)
	}
}

// Future intelligence plugs into the policy, not into matching: a
// replacement GeographicExpansionPolicy changes the scope without any
// change to SupplyListing or the Evaluate contract.
type nationwideImmediately struct{}

func (nationwideImmediately) Scope(f ListingFacts, lc LifecycleView, now time.Time) GeoScope {
	return GeoScope{Stage: GeoRegion, Scope: "COUNTRY", Levels: []geo.Level{geo.LevelCountry}, Reason: "specialist equipment",
		Origin: geo.Place{Resolved: true}}
}

func TestEligibility_PoliciesArePluggable(t *testing.T) {
	p := MustDefaultPolicy().WithParts(nil, nil, nationwideImmediately{})
	e := p.Evaluate(published(pubT0), pubT0)
	if e.Geography.Scope != "COUNTRY" || !e.Eligible {
		t.Fatalf("custom geographic policy: %+v", e.Geography)
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

// A legacy ACTIVE row without published_at never started a lifecycle: it is
// not supply (review finding, 4A.5).
func TestEligibility_ActiveWithoutPublicationIsNotEligible(t *testing.T) {
	f := published(pubT0)
	f.PublishedAt = nil
	e := MustDefaultPolicy().Evaluate(f, pubT0.Add(days(1)))
	if e.Eligible || e.Lifecycle.State != LifecycleNotPublished {
		t.Fatalf("eligible=%v state=%s reasons=%v", e.Eligible, e.Lifecycle.State, e.Reasons)
	}
}
