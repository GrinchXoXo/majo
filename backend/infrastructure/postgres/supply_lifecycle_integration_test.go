package postgres

// Phase 4A.5 Postgres integration tests: lifecycle ceiling, freshness,
// eligibility in matching, history after expiry, no resurrection,
// operator verification, migration 0012.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/supply"
)

// published creates an ACTIVE listing for a fresh signal of vendor v.
func (e *listingEnv) publishedListing(v uuid.UUID, product string, price int64) *supply.Listing {
	e.t.Helper()
	ctx := context.Background()
	l, _, err := e.svc.CreateFromSignal(ctx, e.signal(v, product, price, "", nil), event.ActorSystem, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	p, err := e.svc.Publish(ctx, l.ID, nil, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// age moves a listing's lifecycle into the past by d (publication, ceiling,
// verification and creation), as if it had been published d ago.
func (e *listingEnv) age(id uuid.UUID, d time.Duration) {
	e.t.Helper()
	iv := d.String()
	mustExec(e.t, e.db, `UPDATE supply_listings SET published_at = published_at - $2::interval, expires_at = expires_at - $2::interval,
		last_verified_at = last_verified_at - $2::interval, created_at = created_at - $2::interval WHERE id = $1`, id, iv)
}

func TestIntegration_Lifecycle_PublishSetsCeilingAndRefreshNeverMovesIt(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	v := e.vendorAt("V", "2348051150001", "Benin City")
	l := e.publishedListing(v, "Samsung TV 55", 450000)
	var ok bool
	_ = e.db.QueryRow(`SELECT expires_at = published_at + interval '60 days' FROM supply_listings WHERE id = $1`, l.ID).Scan(&ok)
	if !ok {
		t.Fatal("publication must set expires_at = published_at + 60 days")
	}
	// Vendor posts the same TV again 10 days later: verification only.
	e.age(l.ID, 10*24*time.Hour)
	before, _ := e.listings.FindByID(ctx, l.ID)
	again, outcome, err := e.svc.CreateFromSignal(ctx, e.signal(v, "Samsung TV 55", 450000, "", nil), event.ActorSystem, nil)
	if err != nil || outcome != supply.ListingRefreshed || again.ID != l.ID {
		t.Fatalf("same supply -> refresh: %s %v", outcome, err)
	}
	if !again.ExpiresAt.Equal(before.ExpiresAt) || !again.LastVerifiedAt.After(*before.LastVerifiedAt) {
		t.Fatalf("refresh moves last_verified_at only: ceiling %v -> %v", before.ExpiresAt, again.ExpiresAt)
	}
	if e.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_LISTING_DUPLICATE_REFRESHED' AND entity_id = $1
		AND payload->>'verification' = 'vendor_resubmission'`, l.ID) != 1 {
		t.Fatal("the refresh event records the verification source")
	}
	elig := e.svc.Policy().EvaluateListing(again, nil, time.Now())
	if !elig.Eligible || elig.Freshness.State != supply.Fresh || elig.Lifecycle.Day != 10 || elig.Geography.Stage != supply.GeoLocal {
		t.Fatalf("day 10, re-verified: %+v", elig)
	}
}

func TestIntegration_Lifecycle_ExpiredListingIsHistoryNotSupply(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	e.routing.SetListingEvidence(e.listings, nil)
	_, l := e.draftFromWhatsApp("2348051150002", "EX") // photos + caption: has media
	if _, err := e.svc.Publish(ctx, l.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.db, `INSERT INTO vendor_categories (vendor_id, category) VALUES ($1, 'electronics')`, l.VendorID)
	// The raw signal's own evidence window (7 days, unrelated to the
	// listing lifecycle) will have passed by day 61 as well.
	mustExec(t, e.db, `UPDATE supply_signals SET expires_at = now() - interval '1 minute' WHERE id = $1`, *l.SupplySignalID)
	evidenceBefore := e.evidence(l)
	req := e.confirmedRequest("electronics", "iPhone 13", "128GB", "Benin City", 700000)

	cs, _, _ := e.routing.SuggestVendors(ctx, req)
	if c := byVendor(cs)[l.VendorID]; c.RelevantListing == nil {
		t.Fatal("day 0: the published listing is evidence")
	}
	e.age(l.ID, 61*24*time.Hour) // day 61: past the 60-day ceiling, status still ACTIVE
	cur, _ := e.listings.FindByID(ctx, l.ID)
	if cur.Status != supply.ListingActive {
		t.Fatal("no job rewrites the status")
	}
	if el := e.svc.Policy().EvaluateListing(cur, nil, time.Now()); el.Eligible || el.Lifecycle.State != supply.LifecycleExpired {
		t.Fatalf("logically expired: %+v", el)
	}

	// Cannot match.
	cs, _, _ = e.routing.SuggestVendors(ctx, req)
	if c, ok := byVendor(cs)[l.VendorID]; ok && (c.RelevantListing != nil || c.RelevantSupply != nil) {
		t.Fatalf("an expired listing must not match: %+v", c)
	}
	// Remains queryable as history, with its media and provenance.
	v, err := e.listings.CurationView(ctx, l.ID)
	if err != nil || v == nil || len(v.Media) != 3 || !v.Media[0].Available() || v.Signal == nil || len(v.Sources) != 4 {
		t.Fatalf("expired listing must stay fully inspectable: %+v %v", v, err)
	}
	live, _ := e.listings.Search(ctx, supply.ListingQuery{})
	hist, _ := e.listings.Search(ctx, supply.ListingQuery{Lifecycle: "expired"})
	if len(live) != 0 || len(hist) != 1 || hist[0].ID != l.ID {
		t.Fatalf("search: live=%d expired=%d", len(live), len(hist))
	}
	if e.count(`SELECT count(*) FROM media_assets WHERE owner_vendor_id = $1 AND status = 'AVAILABLE'`, l.VendorID) != 3 {
		t.Fatal("media must stay intact")
	}
	if e.evidence(l) != evidenceBefore {
		t.Fatal("signal / messages / media rows must not change")
	}

	// Cannot be silently resurrected.
	if _, err := e.svc.Verify(ctx, l.ID, nil, nil, nil); !errors.Is(err, supply.ErrLifecycleExpired) {
		t.Fatalf("verify: %v", err)
	}
	if _, err := e.svc.Edit(ctx, l.ID, supply.ListingEdit{Price: i64(1)}, nil); !errors.Is(err, supply.ErrLifecycleExpired) {
		t.Fatalf("edit: %v", err)
	}
	if _, err := e.svc.Publish(ctx, l.ID, nil, nil); err != nil {
		t.Fatalf("re-publish of an ACTIVE listing stays a no-op: %v", err)
	}
	// The vendor posts the same item again: a NEW draft, a new lifecycle.
	newSig := e.signal(l.VendorID, "iphone 13", 620000, "", func(s *supply.Signal) { s.Variant = strPtr("128GB") })
	fresh, outcome, err := e.svc.CreateFromSignal(ctx, newSig, event.ActorSystem, nil)
	if err != nil || outcome != supply.ListingCreated || fresh.ID == l.ID || fresh.Status != supply.ListingDraft {
		t.Fatalf("repost after expiry must create a new draft: %s %v", outcome, err)
	}
	after, _ := e.listings.FindByID(ctx, l.ID)
	if !after.ExpiresAt.Equal(cur.ExpiresAt) || !after.LastVerifiedAt.Equal(*cur.LastVerifiedAt) || after.Status != supply.ListingActive {
		t.Fatal("the expired listing's history must be untouched")
	}
}

func TestIntegration_Lifecycle_MatchingEligibilityByStatus(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	e.routing.SetListingEvidence(e.listings, nil)
	n := 0
	// Each scenario gets its own product, so its vendor is the only one with
	// product evidence for that request.
	scenario := func(name string, setup func(l *supply.Listing)) (uuid.UUID, string) {
		n++
		product := fmt.Sprintf("Console model %c", 'A'+n)
		v := e.vendorAt(name, fmt.Sprint("23480511501", 10+n), "Benin City")
		mustExec(t, e.db, `INSERT INTO vendor_categories (vendor_id, category) VALUES ($1, 'electronics')`, v)
		l := e.publishedListing(v, product, 450000)
		if setup != nil {
			setup(l)
		}
		return v, product
	}
	type expect struct {
		name    string
		setup   func(l *supply.Listing)
		listing bool
	}
	cases := []expect{
		{"ACTIVE inside lifecycle", nil, true},
		{"ACTIVE but stale", func(l *supply.Listing) {
			mustExec(t, e.db, `UPDATE supply_listings SET last_verified_at = now() - interval '5 days' WHERE id = $1`, l.ID)
		}, true},
		{"ACTIVE at exactly day 60", func(l *supply.Listing) { e.age(l.ID, 60*24*time.Hour) }, false},
		// The SQL pre-filter lets this one through; only the policy rejects it.
		{"ACTIVE without a price", func(l *supply.Listing) {
			mustExec(t, e.db, `UPDATE supply_listings SET price = NULL WHERE id = $1`, l.ID)
		}, false},
		{"PAUSED", func(l *supply.Listing) {
			mustExec(t, e.db, `UPDATE supply_listings SET status = 'PAUSED' WHERE id = $1`, l.ID)
		}, false},
		{"UNAVAILABLE", func(l *supply.Listing) {
			mustExec(t, e.db, `UPDATE supply_listings SET status = 'UNAVAILABLE' WHERE id = $1`, l.ID)
		}, false},
		{"SOLD", func(l *supply.Listing) {
			mustExec(t, e.db, `UPDATE supply_listings SET status = 'SOLD' WHERE id = $1`, l.ID)
		}, false},
		{"EXPIRED status", func(l *supply.Listing) {
			mustExec(t, e.db, `UPDATE supply_listings SET status = 'EXPIRED' WHERE id = $1`, l.ID)
		}, false},
		{"REJECTED", func(l *supply.Listing) {
			if _, err := e.svc.Reject(ctx, l.ID, nil, nil, nil); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"ARCHIVED (not rejected)", func(l *supply.Listing) {
			mustExec(t, e.db, `UPDATE supply_listings SET status = 'ARCHIVED' WHERE id = $1`, l.ID)
		}, false},
	}
	for _, c := range cases {
		v, product := scenario(c.name, c.setup)
		req := e.confirmedRequest("electronics", product, "", "Benin City", 500000)
		cs, _, err := e.routing.SuggestVendors(ctx, req)
		if err != nil {
			t.Fatal(err)
		}
		got := byVendor(cs)[v]
		if (got.RelevantListing != nil) != c.listing {
			t.Fatalf("%s: listing evidence = %v, want %v", c.name, got.RelevantListing != nil, c.listing)
		}
	}
	// DRAFT: never listing evidence; its signal still counts as before.
	d := e.vendorAt("Draft", "2348051150199", "Benin City")
	mustExec(t, e.db, `INSERT INTO vendor_categories (vendor_id, category) VALUES ($1, 'electronics')`, d)
	if _, _, err := e.svc.CreateFromSignal(ctx, e.signal(d, "Console model Z", 450000, "", nil), event.ActorSystem, nil); err != nil {
		t.Fatal(err)
	}
	cs, _, _ := e.routing.SuggestVendors(ctx, e.confirmedRequest("electronics", "Console model Z", "", "Benin City", 500000))
	if got := byVendor(cs)[d]; got.RelevantListing != nil || got.RelevantSupply == nil {
		t.Fatalf("DRAFT: %+v", got)
	}
}

func TestIntegration_Lifecycle_OperatorVerifyOverHTTP(t *testing.T) {
	e := newListingEnv(t)
	v := e.vendorAt("V", "2348051150020", "Port Harcourt")
	l := e.publishedListing(v, "Generator 5kva", 380000)
	mustExec(t, e.db, `UPDATE supply_listings SET last_verified_at = now() - interval '100 hours' WHERE id = $1`, l.ID)
	c := e.server(testOperatorToken)
	base := "/api/supply-listings/" + l.ID.String()

	code, d, _ := c.do("GET", base, testOperatorToken, nil)
	el := d["eligibility"].(map[string]any)
	if code != 200 || el["freshness"].(map[string]any)["state"] != "STALE" || el["eligible"] != true ||
		el["geography"].(map[string]any)["stage"] != "LOCAL" || el["lifecycle"].(map[string]any)["max_days"].(float64) != 60 {
		t.Fatalf("detail eligibility: %d %v", code, el)
	}
	if origin := el["geography"].(map[string]any)["origin"].(map[string]any); origin["state"] != "Rivers" {
		t.Fatalf("origin resolved: %v", origin)
	}
	if code, _, _ := c.do("POST", base+"/verify", "", map[string]any{}); code != 401 {
		t.Fatalf("verify without token: %d", code)
	}
	code, body, _ := c.do("POST", base+"/verify", testOperatorToken, map[string]any{"note": "vendor confirmed by phone"})
	if code != 200 || body["eligibility"].(map[string]any)["freshness"].(map[string]any)["state"] != "FRESH" {
		t.Fatalf("verify: %d %v", code, body)
	}
	if e.count(`SELECT count(*) FROM audit_logs WHERE action = 'SUPPLY_LISTING_VERIFIED' AND entity_id = $1`, l.ID) != 1 ||
		e.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_LISTING_VERIFIED' AND entity_id = $1`, l.ID) != 1 {
		t.Fatal("verification is audited and evented once")
	}
	var same bool
	_ = e.db.QueryRow(`SELECT expires_at = published_at + interval '60 days' FROM supply_listings WHERE id = $1`, l.ID).Scan(&same)
	if !same {
		t.Fatal("verification must not move the lifecycle ceiling")
	}
	// Reads never verify.
	for i := 0; i < 3; i++ {
		c.do("GET", base, testOperatorToken, nil)
		c.do("GET", "/api/supply-listings?status=ACTIVE", testOperatorToken, nil)
	}
	if e.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_LISTING_VERIFIED'`) != 1 {
		t.Fatal("reads must not create verification events")
	}
	// Expired -> 409 LIFECYCLE_EXPIRED.
	e.age(l.ID, 61*24*time.Hour)
	if code, body, _ := c.do("POST", base+"/verify", testOperatorToken, map[string]any{}); code != 409 || body["code"] != "LIFECYCLE_EXPIRED" {
		t.Fatalf("verify expired: %d %v", code, body)
	}
	// The lifecycle filter on search: the expired listing is history.
	if code, _, raw := c.do("GET", "/api/supply-listings?status=ACTIVE&lifecycle=expired", testOperatorToken, nil); code != 200 ||
		!strings.Contains(string(raw), l.PublicReference) {
		t.Fatalf("lifecycle=expired: %d %s", code, raw)
	}
	if code, _, raw := c.do("GET", "/api/supply-listings?status=ACTIVE", testOperatorToken, nil); code != 200 ||
		strings.Contains(string(raw), l.PublicReference) {
		t.Fatalf("default (live) search must not show it: %d", code)
	}
	if code, _, _ := c.do("GET", "/api/supply-listings?lifecycle=bogus", testOperatorToken, nil); code != 400 {
		t.Fatalf("bad lifecycle filter: %d", code)
	}
}

func TestIntegration_Migration0012BackfillsTheCeiling(t *testing.T) {
	pre := t.TempDir()
	entries, _ := os.ReadDir(migrationsPath())
	for _, en := range entries {
		if en.Name() >= "0012" {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(migrationsPath(), en.Name()))
		_ = os.WriteFile(filepath.Join(pre, en.Name()), b, 0o644)
	}
	db := testDB(t, pre)
	v := uuid.New()
	mustExec(t, db, `INSERT INTO vendors (id, business_name, whatsapp_external_id, status) VALUES ($1, 'Old', '2348051159999', 'ACTIVE')`, v)
	active, draft, sold, lapsed := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	// The old 7-day semantics: expires 7 days after the evidence.
	mustExec(t, db, `INSERT INTO supply_listings (id, vendor_id, product, status, published_at, created_at, expires_at, public_reference)
		VALUES ($1, $2, 'TV', 'ACTIVE', now() - interval '5 days', now() - interval '6 days', now() + interval '1 day', 'L-AAAAA1')`, active, v)
	// Already past its old ceiling: history, must NOT be resurrected.
	mustExec(t, db, `INSERT INTO supply_listings (id, vendor_id, product, status, published_at, created_at, expires_at, public_reference)
		VALUES ($1, $2, 'Radio', 'ACTIVE', now() - interval '9 days', now() - interval '10 days', now() - interval '3 days', 'L-AAAAA4')`, lapsed, v)
	mustExec(t, db, `INSERT INTO supply_listings (id, vendor_id, product, status, created_at, expires_at, public_reference)
		VALUES ($1, $2, 'Fan', 'DRAFT', now() - interval '2 days', now() + interval '5 days', 'L-AAAAA2')`, draft, v)
	mustExec(t, db, `INSERT INTO supply_listings (id, vendor_id, product, status, published_at, created_at, expires_at, public_reference)
		VALUES ($1, $2, 'Bag', 'SOLD', now() - interval '9 days', now() - interval '10 days', now() - interval '3 days', 'L-AAAAA3')`, sold, v)
	var soldBefore time.Time
	_ = db.QueryRow(`SELECT expires_at FROM supply_listings WHERE id = $1`, sold).Scan(&soldBefore)
	if err := RunMigrations(context.Background(), db, migrationsPath()); err != nil {
		t.Fatalf("0012: %v", err)
	}
	check := func(id uuid.UUID, q string) {
		t.Helper()
		var ok bool
		if err := db.QueryRow(`SELECT `+q+` FROM supply_listings WHERE id = $1`, id).Scan(&ok); err != nil || !ok {
			t.Fatalf("%s: %v", q, err)
		}
	}
	check(active, `expires_at = published_at + interval '60 days'`)
	check(draft, `expires_at = created_at + interval '60 days'`)
	check(lapsed, `expires_at < now()`)
	var soldAfter time.Time
	_ = db.QueryRow(`SELECT expires_at FROM supply_listings WHERE id = $1`, sold).Scan(&soldAfter)
	if !soldAfter.Equal(soldBefore) {
		t.Fatal("closed rows are history: untouched")
	}
	var def string
	_ = db.QueryRow(`SELECT column_default FROM information_schema.columns WHERE table_name = 'supply_listings' AND column_name = 'expires_at'`).Scan(&def)
	if def == "" || !strings.Contains(def, "60 days") {
		t.Fatalf("column default must no longer encode 7 days: %q", def)
	}
}
