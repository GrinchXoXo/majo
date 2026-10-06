package postgres

// Phase 4A (Architecture Lock V2 §M-4A.6): matching reads ACTIVE
// SupplyListings as supply evidence alongside fresh SupplySignals. Real
// PostgreSQL, real routing.Service.SuggestVendors, real curation.

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/matching"
	"github.com/middleman/backend/internal/supply"
)

func (e *listingEnv) confirmedRequest(category, product, variant, location string, budgetMax int64) uuid.UUID {
	b, c, r := uuid.New(), uuid.New(), uuid.New()
	mustExec(e.t, e.db, `INSERT INTO buyers (id, external_user_id) VALUES ($1, $2)`, b, "buyer-"+r.String()[:8])
	mustExec(e.t, e.db, `INSERT INTO conversations (id, external_id, buyer_id) VALUES ($1, $2, $3)`, c, "buyer-"+r.String()[:8], b)
	mustExec(e.t, e.db, `INSERT INTO buyer_requests (id, conversation_id, buyer_id, status, category, product, variant, location, budget_max, raw_message, confirmed_at)
		VALUES ($1, $2, $3, 'CONFIRMED', $4, $5, $6, $7, $8, 'need it', now())`, r, c, b, category, product, variant, location, budgetMax)
	return r
}

func byVendor(cs []matching.Candidate) map[uuid.UUID]matching.Candidate {
	m := map[uuid.UUID]matching.Candidate{}
	for _, c := range cs {
		m[c.Vendor.ID] = c
	}
	return m
}

func TestIntegration_Matching_UsesPublishedListings(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	mk := func(name, wa string) (uuid.UUID, *supply.Listing) {
		v := e.vendorAt(name, wa, "Benin City")
		mustExec(t, e.db, `INSERT INTO vendor_categories (vendor_id, category) VALUES ($1, 'electronics')`, v)
		sig := e.signal(v, "iPhone 13", 620000, "", func(s *supply.Signal) { s.Variant = strPtr("128GB") })
		l, _, err := e.svc.CreateFromSignal(ctx, sig, event.ActorSystem, nil)
		if err != nil {
			t.Fatal(err)
		}
		return v, l
	}
	published, lp := mk("Published", "2348051130001")
	rejected, lr := mk("Rejected", "2348051130002")
	drafted, _ := mk("Draft only", "2348051130003")

	// Operator corrects the display product and the price, then publishes.
	if _, err := e.svc.Edit(ctx, lp.ID, supply.ListingEdit{Product: strPtr("Apple iPhone 13 128GB"), Price: i64(600000)}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Publish(ctx, lp.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Reject(ctx, lr.ID, strPtr("not a real offer"), nil, nil); err != nil {
		t.Fatal(err)
	}
	req := e.confirmedRequest("electronics", "iPhone 13", "128GB", "Benin City", 610000)

	// Before: signals only (no listing evidence wired).
	cs, _, err := e.routing.SuggestVendors(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	before := byVendor(cs)
	if before[published].RelevantSupply == nil || before[published].RelevantListing != nil || *before[published].WithinBudget {
		t.Fatalf("baseline: signal evidence at the raw 620000 price: %+v", before[published])
	}

	// After: matching reads ACTIVE listings.
	e.routing.SetListingEvidence(e.listings)
	cs, _, err = e.routing.SuggestVendors(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	after := byVendor(cs)

	p := after[published]
	if p.RelevantListing == nil || p.RelevantListing.ID != lp.ID || p.RelevantSupply != nil {
		t.Fatalf("the published vendor is ranked on its listing: %+v", p)
	}
	if p.MatchType != matching.MatchSupplyConfirmed || p.Breakdown["product"] != 30 || p.Breakdown["availability"] != 10 {
		t.Fatalf("the corrected listing still matches 'iPhone 13' exactly via its source product: %v %s", p.Breakdown, p.MatchType)
	}
	if p.WithinBudget == nil || !*p.WithinBudget {
		t.Fatal("budget uses the operator-corrected price (600000 <= 610000)")
	}

	r := after[rejected]
	if r.RelevantSupply != nil || r.RelevantListing != nil || r.Breakdown["product"] != 0 {
		t.Fatalf("a rejected listing's signal is no longer supply evidence: %+v", r)
	}

	d := after[drafted]
	if d.RelevantSupply == nil || d.RelevantListing != nil || d.Breakdown["product"] != 30 {
		t.Fatalf("an uncurated (DRAFT) signal stays signal evidence, as before: %+v", d)
	}

	if after[published].Score < after[rejected].Score {
		t.Fatal("listing evidence must not rank below no evidence")
	}
	var payload string
	_ = e.db.QueryRow(`SELECT payload::text FROM events WHERE event_type = 'VENDORS_SUGGESTED' AND entity_id = $1
		ORDER BY created_at DESC LIMIT 1`, req).Scan(&payload)
	if payload == "" || !strings.Contains(payload, `"listing_evidence_count": 1`) {
		t.Fatalf("VENDORS_SUGGESTED records how many candidates rest on listing evidence: %s", payload)
	}

	// Matching never sends or creates anything.
	if e.count(`SELECT count(*) FROM vendor_requests`)+e.count(`SELECT count(*) FROM offers`)+
		e.count(`SELECT count(*) FROM messages WHERE direction = 'OUTBOUND'`) != 0 {
		t.Fatal("matching must not create vendor requests, offers or messages")
	}
}

func TestIntegration_Matching_ExpiredOrUnpublishedListingsIgnored(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	e.routing.SetListingEvidence(e.listings)
	v := e.vendorAt("V", "2348051130010", "Benin City")
	mustExec(t, e.db, `INSERT INTO vendor_categories (vendor_id, category) VALUES ($1, 'electronics')`, v)
	sig := e.signal(v, "PS5", 450000, "", nil)
	l, _, _ := e.svc.CreateFromSignal(ctx, sig, event.ActorSystem, nil)
	if _, err := e.svc.Publish(ctx, l.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	// The listing's evidence expires (no expiry worker: status stays ACTIVE).
	mustExec(t, e.db, `UPDATE supply_listings SET expires_at = now() - interval '1 minute' WHERE id = $1`, l.ID)
	ev, err := e.listings.MatchingEvidence(ctx, []uuid.UUID{v})
	if err != nil {
		t.Fatal(err)
	}
	if len(ev.ListingsByVendor[v]) != 0 || !ev.SupersededSignals[sig] {
		t.Fatalf("an expired listing is not evidence, and its signal stays superseded: %+v", ev)
	}
	req := e.confirmedRequest("electronics", "PS5", "", "Benin City", 500000)
	cs, _, err := e.routing.SuggestVendors(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if c := byVendor(cs)[v]; c.RelevantListing != nil || c.Breakdown["product"] != 0 {
		t.Fatalf("expired listing must not be evidence: %+v", c)
	}
}
