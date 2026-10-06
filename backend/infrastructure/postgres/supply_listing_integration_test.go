package postgres

// Phase 4A.3 Postgres integration tests: SupplySignal -> SupplyListing.
// Real PostgreSQL, the real webhook/routing/intake/worker pipeline from
// 4A.1-4A.2, and the real ListingService + SupplyListingRepository.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/media"
	"github.com/middleman/backend/internal/supply"
)

var refPattern = regexp.MustCompile(`^L-[0-9A-HJKMNP-TV-Z]{6}$`)

type listingEnv struct {
	*supplyEnv
	listings *SupplyListingRepository
	svc      *supply.ListingService
}

func newListingEnv(t *testing.T) *listingEnv {
	t.Helper()
	s := newSupplyEnv(t)
	e := &listingEnv{supplyEnv: s, listings: NewSupplyListingRepository(s.db)}
	e.svc = supply.NewListingService(NewSupplyRepository(s.db), e.listings, NewVendorRepository(s.db), e.listings,
		NewEventWriter(s.db), NewAuditWriter(s.db), discard())
	e.svc.SetDraftJobs(s.jobs, 5)
	s.intake.SetSignalListener(e.svc)
	return e
}

// send is supplyEnv.send plus one more wait: a text handled on the routing
// fast path becomes a signal inside the asynchronous webhook runner, and the
// SUPPLY_LISTING_DRAFT job is enqueued just after the signal commits. Wait
// for that job so a following drain cannot finish in between (a test
// harness race seen under -race load; production picks the job up later).
func (e *listingEnv) send(from, wamid string, at time.Time, kind, body string) {
	e.t.Helper()
	e.supplyEnv.send(from, wamid, at, kind, body)
	if kind != "text" {
		return
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var sig sql.NullString
		_ = e.db.QueryRow(`SELECT ss.supply_signal_id::text FROM supply_signal_sources ss JOIN messages m ON m.id = ss.message_id
			WHERE m.external_message_id = $1`, wamid).Scan(&sig)
		if !sig.Valid {
			return // not (yet) a signal source: a deferred grouping job or a reply; drain covers it
		}
		if e.count(`SELECT count(*) FROM jobs WHERE job_type = $1 AND idempotency_key LIKE $2`,
			supply.JobTypeListingDraft, sig.String+":%") > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	e.t.Fatalf("no listing draft job for the signal of %s", wamid)
}

// worker runs media download, grouping and listing drafts, as the server does.
func (e *listingEnv) worker(id string) *jobs.Worker {
	w := e.fullWorker(id)
	w.Register(supply.JobTypeListingDraft, e.svc.HandleDraftJob, nil)
	return w
}

func (e *listingEnv) vendorAt(name, wa, location string) uuid.UUID {
	id := uuid.New()
	var loc any
	if location != "" {
		loc = location
	}
	mustExec(e.t, e.db, `INSERT INTO vendors (id, business_name, whatsapp_external_id, status, primary_location) VALUES ($1, $2, $3, 'ACTIVE', $4)`,
		id, name, wa, loc)
	return id
}

// signal inserts a signal directly (for field-level cases).
func (e *listingEnv) signal(vendorID uuid.UUID, product string, price int64, location string, mod func(*supply.Signal)) uuid.UUID {
	e.t.Helper()
	s := &supply.Signal{VendorID: vendorID, Price: &price, RawMessage: product, AvailabilityStatus: supply.AvailabilityAvailable,
		Confidence: 0.8, ExtractedData: map[string]any{}}
	if product != "" {
		p := product
		s.Product = &p
	}
	if location != "" {
		l := location
		s.Location = &l
	}
	if mod != nil {
		mod(s)
	}
	out, err := NewSupplyRepository(e.db).Create(context.Background(), s)
	if err != nil {
		e.t.Fatal(err)
	}
	return out.ID
}

func (e *listingEnv) listingsOf(vendorID uuid.UUID) []*supply.Listing {
	e.t.Helper()
	ls, err := e.listings.ListByVendor(context.Background(), vendorID)
	if err != nil {
		e.t.Fatal(err)
	}
	return ls
}

func assetIDsOf(l *supply.Listing) []string {
	out := []string{}
	for _, m := range l.MediaReferences {
		out = append(out, m.AssetID)
	}
	return out
}

// --- A, B, C, F, H, J, K + E2E-shaped path ---------------------------------------

func TestIntegration_Listing_FromWhatsAppSubmissionToDraft(t *testing.T) {
	e := newListingEnv(t)
	wa := "2348051110001"
	vendor := e.vendorAt("Mike Gadgets", wa, "Benin City")
	for i, b := range [][]byte{photo1, photo2, photo3} {
		e.graph.AddMedia(fmt.Sprint("lst-", i+1), b, "image/jpeg")
	}
	t0 := ago(10 * time.Minute)
	e.send(wa, "wamid.LS1", t0, "image:lst-1", "")
	e.send(wa, "wamid.LS2", t0.Add(5*time.Second), "image:lst-2", "")
	e.send(wa, "wamid.LS3", t0.Add(9*time.Second), "image:lst-3", "")
	e.send(wa, "wamid.LS4", t0.Add(15*time.Second), "text", "iPhone 13 128GB ₦620,000 brand new")
	e.drain(e.worker("w"))

	sigs := e.signals(vendor)
	ls := e.listingsOf(vendor)
	if len(sigs) != 1 || len(ls) != 1 {
		t.Fatalf("expected one signal and one listing, got %d / %d", len(sigs), len(ls))
	}
	l := ls[0]
	if l.Status != supply.ListingDraft || l.PublishedAt != nil {
		t.Fatalf("a listing from a signal must be a DRAFT, got %s", l.Status)
	}
	if *l.SupplySignalID != sigs[0].ID || l.VendorID != vendor || *l.Product != "iphone 13" || *l.Category != "electronics" ||
		*l.Price != 620000 || l.Currency != "NGN" || l.Condition != nil || l.Quantity != nil || l.Variant != nil {
		t.Fatalf("mapping wrong (nothing may be invented): %+v", l)
	}
	if *l.Location != "Benin City" {
		t.Fatalf("no location in the evidence -> vendor primary location, got %v", l.Location)
	}
	want := []string{e.assetOf("wamid.LS1").String(), e.assetOf("wamid.LS2").String(), e.assetOf("wamid.LS3").String()}
	if fmt.Sprint(assetIDsOf(l)) != fmt.Sprint(want) {
		t.Fatalf("media references must be the signal's assets in order:\n got %v\nwant %v", assetIDsOf(l), want)
	}
	for _, m := range l.MediaReferences {
		if e.count(`SELECT count(*) FROM media_assets WHERE id = $1 AND status = 'AVAILABLE' AND storage_key = $2 AND kind = $3`,
			m.AssetID, m.StorageKey, m.Type) != 1 {
			t.Fatalf("media reference %+v does not point at a stored asset", m)
		}
	}
	if !refPattern.MatchString(l.PublicReference) {
		t.Fatalf("bad public reference %q", l.PublicReference)
	}
	var sigCreated time.Time
	_ = e.db.QueryRow(`SELECT created_at FROM supply_signals WHERE id = $1`, sigs[0].ID).Scan(&sigCreated)
	if l.LastVerifiedAt == nil || !l.LastVerifiedAt.Equal(sigCreated) || !l.ExpiresAt.Equal(sigCreated.Add(supply.DefaultPolicyConfig().MaxLifecycle)) {
		t.Fatalf("verified = evidence time, provisional ceiling = +60d (4A.5); got %v / %v (signal %v)", l.LastVerifiedAt, l.ExpiresAt, sigCreated)
	}
	if l.SourceMessageID == nil || *l.SourceMessageID != e.messageID("wamid.LS1") {
		t.Fatal("source_message_id must be the first evidence message")
	}
	if e.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_LISTING_CREATED' AND entity_id = $1
		AND payload->>'public_reference' = $2 AND payload->>'location_source' = 'vendor_primary_location'
		AND jsonb_array_length(payload->'media_asset_ids') = 3 AND payload->>'supply_signal_id' = $3`,
		l.ID, l.PublicReference, sigs[0].ID.String()) != 1 {
		t.Fatal("expected one SUPPLY_LISTING_CREATED explaining the listing")
	}
	// "Why does this listing exist?" answered from the database alone.
	if e.count(`SELECT count(*) FROM supply_listings l JOIN supply_signals s ON s.id = l.supply_signal_id
		JOIN supply_signal_sources ss ON ss.supply_signal_id = s.id JOIN messages m ON m.id = ss.message_id
		WHERE l.id = $1 AND s.vendor_id = l.vendor_id`, l.ID) != 4 {
		t.Fatal("listing -> signal -> messages chain broken")
	}
	// Nothing beyond the listing: no publication, offers or buyer requests.
	if e.count(`SELECT count(*) FROM supply_listings WHERE status = 'ACTIVE'`)+e.count(`SELECT count(*) FROM offers`)+
		e.count(`SELECT count(*) FROM buyer_requests`) != 0 {
		t.Fatal("4A.3 must not publish, create offers or buyer requests")
	}
	if e.count(`SELECT count(*) FROM events WHERE event_type IN ('SUPPLY_LISTING_PUBLISHED')`) != 0 {
		t.Fatal("creation is not publication")
	}
}

// --- E, G: location ---------------------------------------------------------------

func TestIntegration_Listing_LocationResolution(t *testing.T) {
	e := newListingEnv(t)
	benin := e.vendorAt("Benin vendor", "2348051110002", "Benin City")
	none := e.vendorAt("No location vendor", "2348051110003", "")
	explicit := e.signal(benin, "PS5", 850000, "Port Harcourt", nil)
	fallback := e.signal(benin, "PS5 Slim", 850000, "", nil)
	empty := e.signal(none, "PS5", 850000, "", nil)
	ctx := context.Background()
	l1, _, err := e.svc.CreateFromSignal(ctx, explicit, event.ActorSystem, nil)
	if err != nil || *l1.Location != "Port Harcourt" {
		t.Fatalf("explicit signal location must win: %v %v", l1, err)
	}
	l2, _, _ := e.svc.CreateFromSignal(ctx, fallback, event.ActorSystem, nil)
	if *l2.Location != "Benin City" {
		t.Fatalf("fallback to vendor primary location, got %v", l2.Location)
	}
	l3, _, _ := e.svc.CreateFromSignal(ctx, empty, event.ActorSystem, nil)
	if l3.Location != nil {
		t.Fatalf("no evidence and no vendor location -> NULL, got %v", *l3.Location)
	}
	if e.count(`SELECT count(*) FROM supply_listings WHERE location IS NULL AND id = $1`, l3.ID) != 1 {
		t.Fatal("location must be stored as NULL, not an invented value")
	}
}

// --- D: unusable media ------------------------------------------------------------

func TestIntegration_Listing_OnlyReferencesUsableMedia(t *testing.T) {
	e := newListingEnv(t)
	wa := "2348051110004"
	v := e.vendorAt("V", wa, "Benin City")
	e.graph.AddMedia("um-1", photo1, "image/jpeg")
	e.graph.AddMedia("um-2", photo2, "image/jpeg")
	t0 := ago(10 * time.Minute)
	e.send(wa, "wamid.UM1", t0, "image:um-1", "")
	e.send(wa, "wamid.UM2", t0.Add(5*time.Second), "image:um-2", "")
	e.send(wa, "wamid.UM3", t0.Add(9*time.Second), "text", "Imported ladies bags ₦18,500")
	// Ingest media and group, but build the listing only after an asset is withdrawn.
	w := e.fullWorker("w")
	e.drainTypes(w, media.JobTypeDownload, supply.JobTypeGroup)
	removed := e.assetOf("wamid.UM2")
	mustExec(t, e.db, `UPDATE media_assets SET status = 'REMOVED' WHERE id = $1`, removed)
	e.drain(e.worker("w2"))
	ls := e.listingsOf(v)
	if len(ls) != 1 || fmt.Sprint(assetIDsOf(ls[0])) != fmt.Sprint([]string{e.assetOf("wamid.UM1").String()}) {
		t.Fatalf("only the usable asset may be referenced, got %+v", ls)
	}
	if e.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_LISTING_CREATED' AND entity_id = $1
		AND payload->'unusable_media_asset_ids' ? $2`, ls[0].ID, removed.String()) != 1 {
		t.Fatal("the skipped asset must be recorded, not silently dropped")
	}
}

// drainTypes runs a worker until no job of the given types is open.
func (e *listingEnv) drainTypes(w *jobs.Worker, types ...string) {
	e.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		ran, err := w.RunOnce(context.Background(), "d")
		if err != nil {
			e.t.Fatal(err)
		}
		if !ran {
			open := 0
			for _, ty := range types {
				open += e.count(`SELECT count(*) FROM jobs WHERE job_type = $1 AND status IN ('PENDING','RUNNING')`, ty)
			}
			if open == 0 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	e.t.Fatal("jobs did not drain")
}

// --- I: public reference collisions -------------------------------------------------

func TestIntegration_Listing_PublicReferenceCollisionRetries(t *testing.T) {
	e := newListingEnv(t)
	v := e.vendorAt("V", "2348051110005", "Benin City")
	first := e.signal(v, "PS5", 850000, "", nil)
	second := e.signal(v, "iPhone 13", 620000, "", nil)
	seq := []string{"L-AAAAAA", "L-AAAAAA", "L-AAAAAA", "L-BBBBBB"}
	calls := 0
	e.listings.newRef = func() (string, error) { r := seq[calls]; calls++; return r, nil }
	ctx := context.Background()
	l1, _, err := e.svc.CreateFromSignal(ctx, first, event.ActorSystem, nil)
	if err != nil || l1.PublicReference != "L-AAAAAA" {
		t.Fatalf("first: %v %v", l1, err)
	}
	l2, outcome, err := e.svc.CreateFromSignal(ctx, second, event.ActorSystem, nil)
	if err != nil || outcome != supply.ListingCreated || l2.PublicReference != "L-BBBBBB" || calls != 4 {
		t.Fatalf("a colliding reference must be retried: %v %v calls=%d", l2, err, calls)
	}
	if _, err := e.db.Exec(`UPDATE supply_listings SET public_reference = 'L-AAAAAA' WHERE id = $1`, l2.ID); err == nil {
		t.Fatal("the database must reject a duplicate public reference")
	}
	if _, err := e.db.Exec(`UPDATE supply_listings SET public_reference = 'L-OOOOOO' WHERE id = $1`, l2.ID); err == nil {
		t.Fatal("the database must reject a malformed public reference")
	}
	// Endless collisions fail loudly rather than looping.
	third := e.signal(v, "Xbox", 500000, "", nil)
	e.listings.newRef = func() (string, error) { return "L-AAAAAA", nil }
	if _, _, err := e.svc.CreateFromSignal(ctx, third, event.ActorSystem, nil); err == nil {
		t.Fatal("expected an error after bounded retries")
	}
	if e.count(`SELECT count(*) FROM supply_listings WHERE supply_signal_id = $1`, third) != 0 {
		t.Fatal("a failed creation must leave nothing behind")
	}
}

// --- L, T: idempotency and concurrency -------------------------------------------

func TestIntegration_Listing_ConcurrentConversionYieldsOneListing(t *testing.T) {
	e := newListingEnv(t)
	v := e.vendorAt("V", "2348051110006", "Benin City")
	sig := e.signal(v, "iPhone 13", 620000, "", nil)
	dupA := e.signal(v, "iPhone 13", 650000, "", nil)
	dupB := e.signal(v, "iphone13", 650000, "", nil) // same normalized product, variant, price as dupA
	var wg sync.WaitGroup
	errs := make(chan error, 30)
	for i := 0; i < 30; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := []uuid.UUID{sig, dupA, dupB}[i%3]
			if _, _, err := e.svc.CreateFromSignal(context.Background(), id, event.ActorSystem, nil); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if n := e.count(`SELECT count(*) FROM supply_listings WHERE supply_signal_id = $1`, sig); n != 1 {
		t.Fatalf("30 concurrent conversions: expected 1 listing for the signal, got %d", n)
	}
	if n := e.count(`SELECT count(*) FROM supply_listings WHERE vendor_id = $1 AND price = 650000`, v); n != 1 {
		t.Fatalf("two duplicate signals converted concurrently: expected 1 listing, got %d", n)
	}
	if n := e.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_LISTING_CREATED'`); n != 2 {
		t.Fatalf("expected exactly 2 creation events, got %d", n)
	}
	if _, err := e.db.Exec(`INSERT INTO supply_listings (vendor_id, supply_signal_id, public_reference) VALUES ($1, $2, 'L-ZZZZZZ')`, v, sig); err == nil {
		t.Fatal("the database must allow only one live listing per signal")
	}
}

// --- M, N: duplicate refresh preserves operator corrections ----------------------

func TestIntegration_Listing_DuplicateRefreshAdvancesFreshnessOnly(t *testing.T) {
	e := newListingEnv(t)
	v := e.vendorAt("V", "2348051110007", "Benin City")
	ctx := context.Background()
	older := e.signal(v, "iPhone 13", 620000, "", func(s *supply.Signal) { s.Variant = strPtr("128GB") })
	mustExec(t, e.db, `UPDATE supply_signals SET created_at = now() - interval '2 days' WHERE id = $1`, older)
	l, _, err := e.svc.CreateFromSignal(ctx, older, event.ActorSystem, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Operator corrections (with the existing audit trail).
	desc, cond, loc := "Sealed, with receipt", "new", "GRA, Benin City"
	if _, err := e.svc.Edit(ctx, l.ID, supply.ListingEdit{Description: &desc, Condition: &cond, Location: &loc}, nil); err != nil {
		t.Fatal(err)
	}
	newer := e.signal(v, "iPhone 13", 620000, "Lagos", func(s *supply.Signal) { s.Variant = strPtr("128GB") })
	refreshed, outcome, err := e.svc.CreateFromSignal(ctx, newer, event.ActorSystem, nil)
	if err != nil || outcome != supply.ListingRefreshed || refreshed.ID != l.ID {
		t.Fatalf("a duplicate signal must refresh the existing listing: %v %s %v", refreshed, outcome, err)
	}
	var newerCreated time.Time
	_ = e.db.QueryRow(`SELECT created_at FROM supply_signals WHERE id = $1`, newer).Scan(&newerCreated)
	// Phase 4A.5: a refresh is a verification. It advances freshness only;
	// the lifecycle ceiling (expires_at) is NOT moved by new evidence.
	if !refreshed.LastVerifiedAt.Equal(newerCreated) || !refreshed.ExpiresAt.Equal(l.ExpiresAt) {
		t.Fatalf("freshness must advance to the newer evidence, the ceiling must not move: %v / %v (was %v)",
			refreshed.LastVerifiedAt, refreshed.ExpiresAt, l.ExpiresAt)
	}
	if *refreshed.Description != desc || *refreshed.Condition != cond || *refreshed.Location != loc ||
		*refreshed.SupplySignalID != older || refreshed.PublicReference != l.PublicReference {
		t.Fatalf("operator corrections and identity must survive a refresh: %+v", refreshed)
	}
	if e.count(`SELECT count(*) FROM supply_listings WHERE vendor_id = $1`, v) != 1 ||
		e.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_LISTING_DUPLICATE_REFRESHED' AND entity_id = $1`, l.ID) != 1 {
		t.Fatal("expected one listing and one refresh event")
	}
	// Older evidence never moves freshness backwards.
	oldest := e.signal(v, "iPhone 13", 620000, "", func(s *supply.Signal) { s.Variant = strPtr("128GB") })
	mustExec(t, e.db, `UPDATE supply_signals SET created_at = now() - interval '5 days' WHERE id = $1`, oldest)
	again, _, _ := e.svc.CreateFromSignal(ctx, oldest, event.ActorSystem, nil)
	if !again.LastVerifiedAt.Equal(newerCreated) {
		t.Fatal("last_verified_at must not move backwards")
	}
}

func strPtr(s string) *string { return &s }

// --- O, P, Q, R: search ---------------------------------------------------------

func TestIntegration_Listing_SearchNormalizationAndStatus(t *testing.T) {
	e := newListingEnv(t)
	v := e.vendorAt("V", "2348051110008", "Benin City")
	ctx := context.Background()
	mk := func(product string, price int64, status supply.ListingStatus) *supply.Listing {
		l, _, err := e.svc.CreateFromSignal(ctx, e.signal(v, product, price, "", nil), event.ActorSystem, nil)
		if err != nil {
			t.Fatal(err)
		}
		if status != supply.ListingDraft {
			// Status fixtures are set directly: ListingFields has no status
			// (transitions only go through Publish/Reject since Step 4).
			mustExec(t, e.db, `UPDATE supply_listings SET status = $2 WHERE id = $1`, l.ID, string(status))
			if l, err = e.listings.FindByID(ctx, l.ID); err != nil {
				t.Fatal(err)
			}
		}
		return l
	}
	active := mk("iPhone 13", 620000, supply.ListingActive)
	mk("iPhone 13", 600000, supply.ListingSold)
	mk("iPhone 13", 610000, supply.ListingArchived)
	draft := mk("iPhone 13", 630000, supply.ListingDraft)
	mk("PS5", 850000, supply.ListingActive)

	ids := func(ls []*supply.Listing) []uuid.UUID {
		out := []uuid.UUID{}
		for _, l := range ls {
			out = append(out, l.ID)
		}
		return out
	}
	for _, q := range []string{"iphone 13", "iphone13", "IPHONE 13", "iPhone 13", "iPhone-13"} {
		res, err := e.listings.Search(ctx, supply.ListingQuery{NormalizedProduct: &q})
		if err != nil || fmt.Sprint(ids(res)) != fmt.Sprint([]uuid.UUID{active.ID}) {
			t.Fatalf("search %q: expected only the ACTIVE iPhone listing, got %v (%v)", q, ids(res), err)
		}
	}
	for _, loc := range []string{"Benin City", "benin city", "BENIN CITY", "  Benin   City "} {
		res, _ := e.listings.Search(ctx, supply.ListingQuery{Location: &loc})
		if len(res) != 2 {
			t.Fatalf("location %q: expected the 2 ACTIVE listings, got %d", loc, len(res))
		}
	}
	lagos := "Lagos"
	if res, _ := e.listings.Search(ctx, supply.ListingQuery{Location: &lagos}); len(res) != 0 {
		t.Fatal("a different location must not match")
	}
	cat := "ELECTRONICS"
	mustExec(t, e.db, `UPDATE supply_listings SET category = 'electronics' WHERE vendor_id = $1`, v)
	if res, _ := e.listings.Search(ctx, supply.ListingQuery{Category: &cat}); len(res) != 2 {
		t.Fatalf("category search is case-insensitive, got %d", len(res))
	}
	// Operator/admin search can still ask for drafts explicitly.
	st, q := supply.ListingDraft, "iphone 13"
	if res, _ := e.listings.Search(ctx, supply.ListingQuery{NormalizedProduct: &q, Status: &st}); len(res) != 1 || res[0].ID != draft.ID {
		t.Fatal("explicit DRAFT search (operator) must still work")
	}
	// Display value stays readable; the search form is separate.
	if *active.Product != "iPhone 13" || *active.NormalizedProduct != "iphone13" {
		t.Fatalf("display/search values: %q / %q", *active.Product, *active.NormalizedProduct)
	}
}

// --- S: invalid signals never become listings ------------------------------------

func TestIntegration_Listing_InvalidSignalsAreRejected(t *testing.T) {
	e := newListingEnv(t)
	wa := "2348051110009"
	v := e.vendorAt("V", wa, "Benin City")
	e.seedActiveRequest(v, "iphone 13", "MM-510009")
	e.graph.AddMedia("iv-1", photo1, "image/jpeg")
	t0 := ago(10 * time.Minute)
	e.send(wa, "wamid.IV1", t0, "text", "Yes boss, I get am")        // reply -> VendorResponse
	e.send(wa, "wamid.IV2", t0.Add(5*time.Second), "image:iv-1", "") // left with the reply (unresolved)
	// Another vendor, no request in context: a greeting and a media-only submission.
	wa2 := "2348051110019"
	v2 := e.vendorAt("V2", wa2, "Benin City")
	e.graph.AddMedia("iv-2", photo2, "image/jpeg")
	e.send(wa2, "wamid.IV3", t0, "text", "hello")
	e.send(wa2, "wamid.IV4", t0.Add(5*time.Minute), "image:iv-2", "")
	e.drain(e.worker("w"))

	if len(e.listingsOf(v))+len(e.listingsOf(v2)) != 0 {
		t.Fatal("no listing may come from a reply, an unresolved photo, 'hello' or media-only content")
	}
	if e.count(`SELECT count(*) FROM supply_signals WHERE vendor_id = $1`, v) != 0 ||
		e.count(`SELECT count(*) FROM supply_signals WHERE vendor_id = $1`, v2) != 2 {
		t.Fatal("the reply makes no signal; 'hello' and the media-only photo remain as signals (evidence), just not listings")
	}
	// The same rules apply to the operator API path.
	hello := e.signal(v, "", 0, "", func(s *supply.Signal) { s.Price = nil })
	var inel supply.ErrIneligible
	if _, err := e.svc.PublishFromSignal(context.Background(), hello, nil); !errors.As(err, &inel) {
		t.Fatalf("operator path must reject an ineligible signal, got %v", err)
	}
	unavailable := e.signal(v, "PS5", 850000, "", func(s *supply.Signal) { s.AvailabilityStatus = supply.AvailabilityUnavailable })
	if _, _, err := e.svc.CreateFromSignal(context.Background(), unavailable, event.ActorSystem, nil); !errors.As(err, &inel) {
		t.Fatal("an UNAVAILABLE signal is not supply")
	}
	expired := e.signal(v, "PS5", 850000, "", func(s *supply.Signal) { s.ExpiresAt = time.Now().Add(-time.Hour) })
	if _, _, err := e.svc.CreateFromSignal(context.Background(), expired, event.ActorSystem, nil); !errors.As(err, &inel) {
		t.Fatal("expired evidence must not be presented as freshly verified")
	}
	mustExec(t, e.db, `UPDATE vendors SET status = 'BLOCKED' WHERE id = $1`, v)
	blocked := e.signal(v, "PS5", 850000, "", nil)
	if _, _, err := e.svc.CreateFromSignal(context.Background(), blocked, event.ActorSystem, nil); !errors.As(err, &inel) {
		t.Fatal("a BLOCKED vendor gets no listing")
	}
}

// --- late photos reach the DRAFT listing of their own signal ---------------------

func TestIntegration_Listing_LatePhotosSyncIntoDraft(t *testing.T) {
	e := newListingEnv(t)
	wa := "2348051110010"
	v := e.vendorAt("V", wa, "Benin City")
	e.graph.AddMedia("lp-1", photo1, "image/jpeg")
	e.graph.AddMedia("lp-2", photo2, "image/jpeg")
	t0 := ago(10 * time.Minute)
	e.send(wa, "wamid.LP1", t0, "text", "Imported ladies bags ₦18,500") // text signal -> draft
	e.drain(e.worker("w1"))
	if ls := e.listingsOf(v); len(ls) != 1 || len(ls[0].MediaReferences) != 0 {
		t.Fatalf("expected a DRAFT without media first, got %+v", ls)
	}
	e.send(wa, "wamid.LP2", t0.Add(30*time.Second), "image:lp-1", "")
	e.send(wa, "wamid.LP3", t0.Add(40*time.Second), "image:lp-2", "")
	e.drain(e.worker("w2"))
	ls := e.listingsOf(v)
	want := []string{e.assetOf("wamid.LP2").String(), e.assetOf("wamid.LP3").String()}
	if len(ls) != 1 || fmt.Sprint(assetIDsOf(ls[0])) != fmt.Sprint(want) {
		t.Fatalf("late photos of the same signal must reach its DRAFT listing in order, got %+v", ls)
	}
	if e.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_LISTING_UPDATED' AND entity_id = $1
		AND payload->>'change' = 'media_added_from_signal'`, ls[0].ID) != 1 {
		t.Fatal("expected one media-sync event")
	}
}

// --- migration 0010 on pre-existing Phase 3 data ---------------------------------

func TestIntegration_Migration0010OnPhase3Listings(t *testing.T) {
	pre := t.TempDir()
	entries, _ := os.ReadDir(migrationsPath())
	for _, en := range entries {
		if en.Name() >= "0010" {
			continue
		}
		b, _ := os.ReadFile(filepath.Join(migrationsPath(), en.Name()))
		_ = os.WriteFile(filepath.Join(pre, en.Name()), b, 0o644)
	}
	db := testDB(t, pre)
	v, sig := uuid.New(), uuid.New()
	mustExec(t, db, `INSERT INTO vendors (id, business_name, whatsapp_external_id, status) VALUES ($1, 'Old', '2348051119999', 'ACTIVE')`, v)
	mustExec(t, db, `INSERT INTO supply_signals (id, vendor_id, product, raw_message) VALUES ($1, $2, 'iPhone 13', 'x')`, sig, v)
	first, twin, other := uuid.New(), uuid.New(), uuid.New()
	mustExec(t, db, `INSERT INTO supply_listings (id, vendor_id, supply_signal_id, product, normalized_product, status, created_at)
		VALUES ($1, $2, $3, 'iPhone-13 Pro', 'iphone-13pro', 'ACTIVE', now() - interval '1 hour')`, first, v, sig)
	mustExec(t, db, `INSERT INTO supply_listings (id, vendor_id, supply_signal_id, product, status) VALUES ($1, $2, $3, 'iPhone 13', 'DRAFT')`, twin, v, sig)
	mustExec(t, db, `INSERT INTO supply_listings (id, vendor_id, product, status) VALUES ($1, $2, 'PS5', 'ACTIVE')`, other, v)
	if err := RunMigrations(context.Background(), db, migrationsPath()); err != nil {
		t.Fatalf("0010 on Phase 3 data: %v", err)
	}
	var refs []string
	rows, _ := db.Query(`SELECT public_reference FROM supply_listings`)
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		refs = append(refs, r)
	}
	rows.Close()
	if len(refs) != 3 || refs[0] == refs[1] || refs[1] == refs[2] || refs[0] == refs[2] {
		t.Fatalf("every existing listing gets a distinct reference, got %v", refs)
	}
	for _, r := range refs {
		if !refPattern.MatchString(r) {
			t.Fatalf("backfilled reference %q has the wrong format", r)
		}
	}
	var st, norm string
	_ = db.QueryRow(`SELECT status, normalized_product FROM supply_listings WHERE id = $1`, twin).Scan(&st, &norm)
	if st != "ARCHIVED" {
		t.Fatalf("the later twin of a signal's listing is archived, got %s", st)
	}
	_ = db.QueryRow(`SELECT status, normalized_product FROM supply_listings WHERE id = $1`, first).Scan(&st, &norm)
	if st != "ACTIVE" || norm != "iphone13pro" {
		t.Fatalf("the original stays as it was, with the new search form: %s %s", st, norm)
	}
	b, _ := json.Marshal(refs)
	if strings.Contains(string(b), "MM-") {
		t.Fatal("listing references must not reuse the MM- namespace")
	}
}
