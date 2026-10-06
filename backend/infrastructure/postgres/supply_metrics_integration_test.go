package postgres

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/funnel"
	"github.com/middleman/backend/internal/httpserver"
	"github.com/middleman/backend/internal/supply"
)

// Phase 4A (Lock V2 §M-4A.7 / §K.2): supply-side rows in the funnel,
// counted from entity tables.
func TestIntegration_SupplyMetrics(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	a := e.vendorAt("A", "2348051140001", "Benin City")
	b := e.vendorAt("B", "2348051140002", "Benin City")
	mustExec(t, e.db, `INSERT INTO vendors (id, business_name, whatsapp_external_id, status) VALUES (gen_random_uuid(), 'Idle', '2348051140003', 'ACTIVE')`)

	// Signals: A x3 (one two days ago, one without a price), B x1.
	s1 := e.signal(a, "iPhone 13", 620000, "", nil)
	s2 := e.signal(a, "PS5", 450000, "", nil)
	mustExec(t, e.db, `UPDATE supply_signals SET created_at = now() - interval '2 days' WHERE id = $1`, s2)
	e.signal(a, "Bag", 1, "", func(s *supply.Signal) { s.Price = nil })
	s4 := e.signal(b, "Laptop", 300000, "", nil)

	// Listings (signal 3 has no price but a product, so it is drafted too).
	l1, _, _ := e.svc.CreateFromSignal(ctx, s1, event.ActorSystem, nil)
	l2, _, _ := e.svc.CreateFromSignal(ctx, s2, event.ActorSystem, nil)
	l4, _, _ := e.svc.CreateFromSignal(ctx, s4, event.ActorSystem, nil)
	drafted := e.count(`SELECT count(*) FROM supply_listings`)
	if _, err := e.svc.Publish(ctx, l1.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Publish(ctx, l2.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Reject(ctx, l2.ID, strPtr("withdrawn"), nil, nil); err != nil { // published, then withdrawn
		t.Fatal(err)
	}
	if _, err := e.svc.Reject(ctx, l4.ID, strPtr("blurry"), nil, nil); err != nil { // draft rejected
		t.Fatal(err)
	}
	// Media fixtures (set after curation: the metric only reads references).
	mustExec(t, e.db, `UPDATE supply_listings SET media_references = '[{"type":"image","asset_id":"x"}]' WHERE id = $1`, l1.ID)
	mustExec(t, e.db, `UPDATE supply_listings SET media_references = '[{"type":"video","asset_id":"y"}]' WHERE id = $1`, l2.ID)
	// l1 is ACTIVE; make its evidence older than the freshness window.
	mustExec(t, e.db, `UPDATE supply_listings SET last_verified_at = now() - interval '100 hours' WHERE id = $1`, l1.ID)

	until := time.Now().Add(time.Minute)
	got, err := NewFunnelRepository(e.db).Supply(ctx, until.Add(-30*24*time.Hour), until, 72*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	m := map[string]funnel.SupplyMetric{}
	for _, x := range got {
		m[x.Key] = x
	}
	of := func(k string) int64 {
		if m[k].Of == nil {
			return -1
		}
		return *m[k].Of
	}
	check := func(k string, v, d int64) {
		t.Helper()
		if m[k].Value != v || of(k) != d {
			t.Errorf("%s = %d/%d, want %d/%d", k, m[k].Value, of(k), v, d)
		}
	}
	check("vendor_submissions", 4, -1)
	check("submissions_per_submitting_vendor", 4, 2)
	check("valid_supply_rate", 3, 4)
	check("vendor_supply_repeat_rate", 1, 2) // A on two days, B on one
	check("listings_drafted", int64(drafted), -1)
	check("media_complete_rate", 1, int64(drafted)) // only l1 has an image (l2 has a video)
	check("listings_published", 2, -1)
	check("listings_rejected", 1, -1)  // l4 (never published)
	check("listings_withdrawn", 1, -1) // l2
	check("listing_approval_rate", 2, 3)
	check("listing_rejection_rate", 1, 3)
	check("stale_listing_rate", 1, 1) // l1 ACTIVE, verified 100h ago > 72h
	check("usable_listings_per_active_vendor", 1, 3)
	check("vendor_response_rate", 0, 0)
	if len(got) != 14 {
		t.Fatalf("want 14 supply rows, got %d", len(got))
	}

	// HTTP: the funnel endpoint carries the supply rows next to the stages.
	h := httpserver.NewFunnelHandlers(NewFunnelRepository(e.db))
	rec := httptest.NewRecorder()
	h.Get(rec, httptest.NewRequest(http.MethodGet, "/api/metrics/funnel?days=30", nil))
	var rep funnel.Report
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &rep) != nil || len(rep.Stages) != 15 || len(rep.Supply) != 14 {
		t.Fatalf("funnel endpoint: %d stages=%d supply=%d", rec.Code, len(rep.Stages), len(rep.Supply))
	}
}
