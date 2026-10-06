package postgres

// Phase 4A Step 4 Postgres integration tests: operator curation of
// SupplyListings (draft queue, detail, edit, publish, reject, media,
// concurrency, refresh-after-correction, HTTP auth/boundaries). Real
// PostgreSQL, the real WhatsApp -> signal -> DRAFT pipeline, the real
// ListingService, repository and HTTP handlers.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/httpserver"
	"github.com/middleman/backend/internal/supply"
)

const testOperatorToken = "test-operator-token-0123456789"

// draftFromWhatsApp runs the real pipeline: 3 photos + caption -> signal -> DRAFT.
func (e *listingEnv) draftFromWhatsApp(wa, tag string) (vendorID uuid.UUID, l *supply.Listing) {
	e.t.Helper()
	vendorID = e.vendorAt("Mike Gadgets "+tag, wa, "Benin City")
	for i, b := range [][]byte{photo1, photo2, photo3} {
		e.graph.AddMedia(fmt.Sprintf("%s-%d", tag, i+1), b, "image/jpeg")
	}
	t0 := ago(10 * time.Minute)
	for i := 0; i < 3; i++ {
		e.send(wa, fmt.Sprintf("wamid.%s%d", tag, i+1), t0.Add(time.Duration(i)*3*time.Second), fmt.Sprintf("image:%s-%d", tag, i+1), "")
	}
	e.send(wa, "wamid."+tag+"T", t0.Add(15*time.Second), "text", "iPhone 13 128GB ₦620,000 brand new")
	e.drain(e.worker("w-" + tag))
	ls := e.listingsOf(vendorID)
	if len(ls) != 1 || ls[0].Status != supply.ListingDraft || len(ls[0].MediaReferences) != 3 {
		e.t.Fatalf("expected one DRAFT with 3 media, got %+v", ls)
	}
	return vendorID, ls[0]
}

// snapshot returns the rows as JSON text, to prove evidence is unchanged.
func (e *listingEnv) snapshot(q string, args ...any) string {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT row_to_json(x)::text FROM (`+q+`) x`, args...)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			e.t.Fatal(err)
		}
		b.WriteString(s + "\n")
	}
	return b.String()
}

func (e *listingEnv) evidence(l *supply.Listing) string {
	return e.snapshot(`SELECT * FROM supply_signals WHERE id = $1`, *l.SupplySignalID) +
		e.snapshot(`SELECT * FROM supply_signal_sources WHERE supply_signal_id = $1 ORDER BY position`, *l.SupplySignalID) +
		e.snapshot(`SELECT m.* FROM messages m JOIN supply_signal_sources s ON s.message_id = m.id WHERE s.supply_signal_id = $1 ORDER BY s.position`, *l.SupplySignalID) +
		e.snapshot(`SELECT * FROM media_assets WHERE owner_vendor_id = $1 ORDER BY id`, l.VendorID)
}

func (e *listingEnv) events(t event.Type, listingID uuid.UUID) int {
	return e.count(`SELECT count(*) FROM events WHERE event_type = $1 AND entity_id = $2`, string(t), listingID)
}

func (e *listingEnv) audits(action string, listingID uuid.UUID) int {
	return e.count(`SELECT count(*) FROM audit_logs WHERE action = $1 AND entity_id = $2`, action, listingID)
}

// --- Test 1: draft queue -----------------------------------------------------------

func TestIntegration_Curation_DraftQueue(t *testing.T) {
	e := newListingEnv(t)
	v := e.vendorAt("Q", "2348051120001", "Benin City")
	ctx := context.Background()
	mk := func(product string, price int64) *supply.Listing {
		l, _, err := e.svc.CreateFromSignal(ctx, e.signal(v, product, price, "", nil), event.ActorSystem, nil)
		if err != nil {
			t.Fatal(err)
		}
		return l
	}
	d1 := mk("Bag one", 1000)
	toPublish := mk("Bag two", 2000)
	toReject := mk("Bag three", 3000)
	sold := mk("Bag four", 4000)
	d2 := mk("Bag five", 5000)
	if _, err := e.svc.Publish(ctx, toPublish.ID, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Reject(ctx, toReject.ID, strPtr("blurry"), nil, nil); err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.db, `UPDATE supply_listings SET status = 'SOLD' WHERE id = $1`, sold.ID)
	// An expired draft is stale history, not work for the operator.
	stale := mk("Bag six", 6000)
	mustExec(t, e.db, `UPDATE supply_listings SET expires_at = now() - interval '1 minute' WHERE id = $1`, stale.ID)

	ids := func(ls []*supply.Listing) []uuid.UUID {
		out := []uuid.UUID{}
		for _, l := range ls {
			out = append(out, l.ID)
		}
		return out
	}
	draft := supply.ListingDraft
	q, err := e.listings.Search(ctx, supply.ListingQuery{Status: &draft})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ids(q)) != fmt.Sprint([]uuid.UUID{d2.ID, d1.ID}) {
		t.Fatalf("draft queue = DRAFT only, created_at DESC: got %v want %v", ids(q), []uuid.UUID{d2.ID, d1.ID})
	}
	def, _ := e.listings.Search(ctx, supply.ListingQuery{})
	if fmt.Sprint(ids(def)) != fmt.Sprint([]uuid.UUID{toPublish.ID}) {
		t.Fatalf("default search = ACTIVE only, got %v", ids(def))
	}
	archived := supply.ListingArchived
	arch, _ := e.listings.Search(ctx, supply.ListingQuery{Status: &archived})
	if len(arch) != 1 || arch[0].ID != toReject.ID || arch[0].RejectedAt == nil || *arch[0].RejectionReason != "blurry" {
		t.Fatalf("rejected listings are filterable separately: %+v", arch)
	}
}

// --- Tests 2, 5, 6, 8, 15, 16, 17 + Example 65 -----------------------------------------

func TestIntegration_Curation_DetailEditPublishKeepsEvidence(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	vendorID, l := e.draftFromWhatsApp("2348051120002", "CD")
	if e.events(event.SupplyListingPublished, l.ID) != 0 {
		t.Fatal("draft creation must not emit SUPPLY_LISTING_PUBLISHED")
	}
	evidenceBefore := e.evidence(l)
	filesBefore := fmt.Sprint(e.files())

	// Detail (Test 2).
	v, err := e.listings.CurationView(ctx, l.ID)
	if err != nil || v == nil {
		t.Fatal(err)
	}
	if v.Vendor == nil || v.Vendor.ID != vendorID || v.Vendor.Status != "ACTIVE" {
		t.Fatalf("vendor: %+v", v.Vendor)
	}
	if v.Signal == nil || v.Signal.ID != *l.SupplySignalID || *v.Signal.Product != "iphone 13" || *v.Signal.Price != 620000 ||
		!strings.Contains(v.Signal.RawMessage, "iPhone 13") {
		t.Fatalf("signal evidence: %+v", v.Signal)
	}
	if len(v.Sources) != 4 || *v.Sources[0].ExternalMessageID != "wamid.CD1" || *v.Sources[3].ExternalMessageID != "wamid.CDT" ||
		v.Sources[0].MediaAssetID == nil || v.Sources[3].MediaAssetID != nil || v.Sources[3].Text == nil {
		t.Fatalf("source messages: %+v", v.Sources)
	}
	if len(v.Media) != 3 || !v.Media[0].Primary || v.Media[1].Primary || !v.Media[0].Available() || v.Media[0].MimeType != "image/jpeg" {
		t.Fatalf("media: %+v", v.Media)
	}

	// Operator correction + primary photo 2 (Example 65), then publish.
	img2 := l.MediaReferences[1].AssetID
	edited, err := e.svc.Edit(ctx, l.ID, supply.ListingEdit{
		Product: strPtr("Apple iPhone 13 128GB"), Price: i64(600000), Location: strPtr("GRA, Benin City"),
		Condition: strPtr("Brand new"), Category: strPtr("phones"), PrimaryAssetID: &img2, ExpectedUpdatedAt: &l.UpdatedAt,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, _ := e.listings.FindByID(ctx, l.ID)
	if *reloaded.Product != "Apple iPhone 13 128GB" || *reloaded.Price != 600000 || *reloaded.Location != "GRA, Benin City" ||
		*reloaded.Condition != "Brand new" || *reloaded.NormalizedProduct != "appleiphone13128gb" || reloaded.Status != supply.ListingDraft {
		t.Fatalf("edit did not persist: %+v", reloaded)
	}
	if p := supply.PrimaryMedia(reloaded.MediaReferences); p == nil || p.AssetID != img2 {
		t.Fatal("primary media did not persist")
	}
	if fmt.Sprint(assetIDsOf(reloaded)) != fmt.Sprint(assetIDsOf(l)) {
		t.Fatal("primary selection must not reorder, add or drop media")
	}
	var prev, next string
	_ = e.db.QueryRow(`SELECT previous_value::text, new_value::text FROM audit_logs WHERE action = 'SUPPLY_LISTING_CORRECTED' AND entity_id = $1`, l.ID).Scan(&prev, &next)
	if !strings.Contains(prev, `"product": "iphone 13"`) || !strings.Contains(next, `"product": "Apple iPhone 13 128GB"`) ||
		!strings.Contains(prev, `"price": 620000`) || !strings.Contains(next, `"media"`) {
		t.Fatalf("audit old/new: %s -> %s", prev, next)
	}

	beforePublish := e.count(`SELECT count(*) FROM offers`) + e.count(`SELECT count(*) FROM buyer_requests`) +
		e.count(`SELECT count(*) FROM vendor_requests`) + e.count(`SELECT count(*) FROM transactions`) +
		e.count(`SELECT count(*) FROM messages WHERE direction = 'OUTBOUND'`)
	pub, err := e.svc.Publish(ctx, l.ID, &edited.UpdatedAt, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pub.Status != supply.ListingActive || pub.PublishedAt == nil || pub.PublicReference != l.PublicReference {
		t.Fatalf("publish: %+v", pub)
	}
	if e.events(event.SupplyListingPublished, l.ID) != 1 || e.audits("SUPPLY_LISTING_PUBLISHED", l.ID) != 1 {
		t.Fatal("want exactly one SUPPLY_LISTING_PUBLISHED event and audit")
	}
	var payload string
	_ = e.db.QueryRow(`SELECT payload::text FROM events WHERE event_type = 'SUPPLY_LISTING_PUBLISHED' AND entity_id = $1`, l.ID).Scan(&payload)
	if !strings.Contains(payload, l.PublicReference) || !strings.Contains(payload, img2) {
		t.Fatalf("publication payload: %s", payload)
	}
	afterPublish := e.count(`SELECT count(*) FROM offers`) + e.count(`SELECT count(*) FROM buyer_requests`) +
		e.count(`SELECT count(*) FROM vendor_requests`) + e.count(`SELECT count(*) FROM transactions`) +
		e.count(`SELECT count(*) FROM messages WHERE direction = 'OUTBOUND'`)
	if afterPublish != beforePublish {
		t.Fatal("publishing must not create offers/requests/transactions or send messages")
	}

	// Idempotent re-publish; edit of ACTIVE keeps published_at (Tests 10, 14, 24).
	again, err := e.svc.Publish(ctx, l.ID, nil, nil)
	if err != nil || !again.PublishedAt.Equal(*pub.PublishedAt) {
		t.Fatalf("re-publish: %v", err)
	}
	if _, err := e.svc.Edit(ctx, l.ID, supply.ListingEdit{Price: i64(605000)}, nil); err != nil {
		t.Fatal(err)
	}
	final, _ := e.listings.FindByID(ctx, l.ID)
	if !final.PublishedAt.Equal(*pub.PublishedAt) || final.Status != supply.ListingActive ||
		e.events(event.SupplyListingPublished, l.ID) != 1 || len(e.listingsOf(vendorID)) != 1 {
		t.Fatal("ACTIVE edits must not reset published_at, re-publish, or create another listing")
	}

	// Evidence untouched throughout (Tests 5, 6, 17).
	if e.evidence(l) != evidenceBefore {
		t.Fatal("source signal / sources / messages / media assets changed")
	}
	if fmt.Sprint(e.files()) != filesBefore {
		t.Fatal("stored media files changed")
	}
	if *final.SupplySignalID != *l.SupplySignalID || final.VendorID != vendorID {
		t.Fatal("provenance changed")
	}
}

// --- Tests 11, 12, 22: reject keeps evidence; rejection sticks --------------------------

func TestIntegration_Curation_RejectKeepsEvidenceAndSticks(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	vendorID, l := e.draftFromWhatsApp("2348051120003", "RJ")
	evidenceBefore := e.evidence(l)
	rej, err := e.svc.Reject(ctx, l.ID, strPtr("product unclear"), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rej.Status != supply.ListingArchived || rej.RejectedAt == nil || *rej.RejectionReason != "product unclear" || len(rej.MediaReferences) != 3 {
		t.Fatalf("reject: %+v", rej)
	}
	for i := 0; i < 3; i++ {
		if _, err := e.svc.Reject(ctx, l.ID, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	again, _ := e.listings.FindByID(ctx, l.ID)
	if !again.RejectedAt.Equal(*rej.RejectedAt) || *again.RejectionReason != "product unclear" ||
		e.events(event.SupplyListingRejected, l.ID) != 1 || e.audits("SUPPLY_LISTING_REJECTED", l.ID) != 1 {
		t.Fatal("repeated rejection must be a no-op")
	}
	if e.evidence(l) != evidenceBefore {
		t.Fatal("rejection changed evidence")
	}
	// The signal gaining late media re-runs CreateFromSignal: the rejected
	// listing must not come back as a new DRAFT.
	got, outcome, err := e.svc.CreateFromSignal(ctx, *l.SupplySignalID, event.ActorSystem, nil)
	if err != nil || outcome != supply.ListingExisting || got.ID != l.ID || got.Status != supply.ListingArchived {
		t.Fatalf("a rejected signal must not be re-drafted: %v %s %v", got, outcome, err)
	}
	if n := len(e.listingsOf(vendorID)); n != 1 {
		t.Fatalf("expected 1 listing, got %d", n)
	}
	// No republish route.
	if _, err := e.svc.Publish(ctx, l.ID, nil, nil); !errors.As(err, new(supply.ErrInvalidTransition)) {
		t.Fatalf("publish after reject: %v", err)
	}
	// The schema enforces "rejected => ARCHIVED".
	if _, err := e.db.Exec(`UPDATE supply_listings SET status = 'ACTIVE' WHERE id = $1`, l.ID); err == nil {
		t.Fatal("a rejected listing must not be ACTIVE (supply_listings_rejected_is_archived)")
	}
}

// --- Test 9 / Example 67 at the database level -----------------------------------------

func TestIntegration_Curation_InvalidDraftCannotPublish(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	v := e.vendorAt("B", "2348051120004", "")
	sig := e.signal(v, "Unclear thing", 5000, "", nil)
	l, _, err := e.svc.CreateFromSignal(ctx, sig, event.ActorSystem, nil)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, e.db, `UPDATE supply_listings SET price = NULL WHERE id = $1`, l.ID)
	mustExec(t, e.db, `UPDATE vendors SET status = 'BLOCKED' WHERE id = $1`, v)
	_, err = e.svc.Publish(ctx, l.ID, nil, nil)
	var perr supply.ErrNotPublishable
	if !errors.As(err, &perr) || len(perr.Reasons) < 2 {
		t.Fatalf("want price + vendor reasons, got %v", err)
	}
	got, _ := e.listings.FindByID(ctx, l.ID)
	if got.Status != supply.ListingDraft || got.PublishedAt != nil || e.events(event.SupplyListingPublished, l.ID) != 0 ||
		e.audits("SUPPLY_LISTING_PUBLISHED", l.ID) != 0 {
		t.Fatal("a refused publication must leave DRAFT and emit nothing")
	}
}

// --- Test 19: concurrent publication -----------------------------------------------------

func TestIntegration_Curation_ConcurrentPublishYieldsOneEvent(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	v := e.vendorAt("C", "2348051120005", "Benin City")
	l, _, err := e.svc.CreateFromSignal(ctx, e.signal(v, "PS5", 450000, "", nil), event.ActorSystem, nil)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := e.svc.Publish(ctx, l.ID, nil, nil)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent publish: %v", err)
		}
	}
	got, _ := e.listings.FindByID(ctx, l.ID)
	if got.Status != supply.ListingActive || got.PublishedAt == nil ||
		e.events(event.SupplyListingPublished, l.ID) != 1 || e.audits("SUPPLY_LISTING_PUBLISHED", l.ID) != 1 {
		t.Fatalf("want ACTIVE with exactly one publication event/audit, got %s / %d / %d", got.Status,
			e.events(event.SupplyListingPublished, l.ID), e.audits("SUPPLY_LISTING_PUBLISHED", l.ID))
	}
}

func TestIntegration_Curation_ConcurrentPublishAndRejectAreCoherent(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	v := e.vendorAt("C2", "2348051120006", "Benin City")
	for round := 0; round < 5; round++ {
		l, _, err := e.svc.CreateFromSignal(ctx, e.signal(v, fmt.Sprint("Item ", round), 1000+int64(round), "", nil), event.ActorSystem, nil)
		if err != nil {
			t.Fatal(err)
		}
		snap := l.UpdatedAt
		var wg sync.WaitGroup
		var pubErr, rejErr error
		wg.Add(2)
		go func() { defer wg.Done(); _, pubErr = e.svc.Publish(ctx, l.ID, &snap, nil) }()
		go func() { defer wg.Done(); _, rejErr = e.svc.Reject(ctx, l.ID, nil, &snap, nil) }()
		wg.Wait()
		got, _ := e.listings.FindByID(ctx, l.ID)
		pubs, rejs := e.events(event.SupplyListingPublished, l.ID), e.events(event.SupplyListingRejected, l.ID)
		switch {
		case pubErr == nil && errors.Is(rejErr, supply.ErrStale):
			if got.Status != supply.ListingActive || pubs != 1 || rejs != 0 {
				t.Fatalf("publish won: %s %d %d", got.Status, pubs, rejs)
			}
		case rejErr == nil && errors.Is(pubErr, supply.ErrStale):
			if got.Status != supply.ListingArchived || pubs != 0 || rejs != 1 {
				t.Fatalf("reject won: %s %d %d", got.Status, pubs, rejs)
			}
		default:
			t.Fatalf("exactly one of publish/reject must win: %v / %v", pubErr, rejErr)
		}
	}
}

// --- Test 18 / Example 69: correction survives duplicate refresh -----------------------

func TestIntegration_Curation_CorrectionSurvivesDuplicateRefresh(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	v := e.vendorAt("R", "2348051120007", "Benin City")
	s1 := e.signal(v, "iPhone 13", 620000, "", nil)
	mustExec(t, e.db, `UPDATE supply_signals SET created_at = now() - interval '2 days' WHERE id = $1`, s1)
	l, _, err := e.svc.CreateFromSignal(ctx, s1, event.ActorSystem, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Edit(ctx, l.ID, supply.ListingEdit{Product: strPtr("Apple iPhone 13 128GB"), Price: i64(600000)}, nil); err != nil {
		t.Fatal(err)
	}
	pub, err := e.svc.Publish(ctx, l.ID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// The vendor posts the same thing again, in its original words.
	s2 := e.signal(v, "iPhone 13", 620000, "", nil)
	got, outcome, err := e.svc.CreateFromSignal(ctx, s2, event.ActorSystem, nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome != supply.ListingRefreshed || got.ID != l.ID {
		t.Fatalf("the corrected listing must be recognised and refreshed, got %s for %v", outcome, got.ID)
	}
	if *got.Product != "Apple iPhone 13 128GB" || *got.Price != 600000 || got.Status != supply.ListingActive ||
		!got.PublishedAt.Equal(*pub.PublishedAt) || !got.LastVerifiedAt.After(*l.LastVerifiedAt) {
		t.Fatalf("refresh must keep corrections/publication and advance freshness: %+v", got)
	}
	if n := len(e.listingsOf(v)); n != 1 {
		t.Fatalf("no duplicate listing may be created, got %d", n)
	}
	if e.events(event.SupplyListingPublished, l.ID) != 1 {
		t.Fatal("a refresh must not re-publish")
	}
	// Matching on the corrected values still works too.
	s3 := e.signal(v, "Apple iPhone 13 128GB", 600000, "", nil)
	if got, outcome, _ := e.svc.CreateFromSignal(ctx, s3, event.ActorSystem, nil); outcome != supply.ListingRefreshed || got.ID != l.ID {
		t.Fatalf("corrected values: %s", outcome)
	}
	// Different supply still makes a new draft.
	s4 := e.signal(v, "iPhone 13", 700000, "", nil)
	if _, outcome, _ := e.svc.CreateFromSignal(ctx, s4, event.ActorSystem, nil); outcome != supply.ListingCreated {
		t.Fatalf("a different price is different supply: %s", outcome)
	}
}

// --- Test 7: primary media + ordering + unavailable media -------------------------------

func TestIntegration_Curation_PrimaryMediaRules(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	_, l := e.draftFromWhatsApp("2348051120008", "PM")
	_, other := e.draftFromWhatsApp("2348051120009", "PN")
	foreign := other.MediaReferences[0].AssetID
	_, err := e.svc.Edit(ctx, l.ID, supply.ListingEdit{PrimaryAssetID: &foreign}, nil)
	if !errors.As(err, new(supply.ValidationError)) {
		t.Fatalf("another listing's asset cannot become primary: %v", err)
	}
	order := []string{l.MediaReferences[2].AssetID, l.MediaReferences[0].AssetID, foreign}
	if _, err := e.svc.Edit(ctx, l.ID, supply.ListingEdit{MediaOrder: order}, nil); !errors.As(err, new(supply.ValidationError)) {
		t.Fatalf("an unrelated asset cannot be added through ordering: %v", err)
	}
	order = []string{l.MediaReferences[2].AssetID, l.MediaReferences[0].AssetID, l.MediaReferences[1].AssetID}
	got, err := e.svc.Edit(ctx, l.ID, supply.ListingEdit{MediaOrder: order}, nil)
	if err != nil || fmt.Sprint(assetIDsOf(got)) != fmt.Sprint(order) {
		t.Fatalf("reorder: %v %v", err, assetIDsOf(got))
	}
	if supply.PrimaryMedia(got.MediaReferences).AssetID != order[0] {
		t.Fatal("without an explicit primary, the first image is primary")
	}
	// A primary that became unavailable blocks publication until changed.
	mustExec(t, e.db, `UPDATE media_assets SET status = 'REMOVED' WHERE id = $1`, order[0])
	if _, err := e.svc.Publish(ctx, l.ID, nil, nil); !errors.As(err, new(supply.ErrNotPublishable)) {
		t.Fatalf("unavailable primary must block: %v", err)
	}
	v, _ := e.listings.CurationView(ctx, l.ID)
	if v.Media[0].Available() || v.Media[0].Status != "REMOVED" || len(v.Media) != 3 {
		t.Fatal("an unavailable asset must stay listed and be shown as unavailable")
	}
	if _, err := e.svc.Edit(ctx, l.ID, supply.ListingEdit{PrimaryAssetID: &order[1]}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Publish(ctx, l.ID, nil, nil); err != nil {
		t.Fatalf("with an available primary the listing publishes: %v", err)
	}
}

// --- HTTP: auth (Test 13), PATCH boundary, errors, media preview -----------------------

type httpClient struct {
	t   *testing.T
	srv *httptest.Server
}

func (c httpClient) do(method, path, token string, body any) (int, map[string]any, []byte) {
	c.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, c.srv.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	return resp.StatusCode, m, raw
}

func (e *listingEnv) server(token string) httpClient {
	auth := httpserver.NewOperatorAuth(token)
	h := httpserver.NewSupplyListingHandlers(e.listings, e.svc, e.listings, e.store, auth, discard())
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/supply-listings", auth.Require(h.CreateFromSignal))
	mux.HandleFunc("GET /api/supply-listings", auth.Require(h.Search))
	mux.HandleFunc("GET /api/vendors/{id}/supply-listings", auth.Require(h.ListByVendor))
	mux.HandleFunc("GET /api/supply-listings/{id}", auth.Require(h.Get))
	mux.HandleFunc("PATCH /api/supply-listings/{id}", auth.Require(h.Patch))
	mux.HandleFunc("POST /api/supply-listings/{id}/publish", auth.Require(h.PublishListing))
	mux.HandleFunc("POST /api/supply-listings/{id}/activate", auth.Require(h.PublishListing))
	mux.HandleFunc("POST /api/supply-listings/{id}/reject", auth.Require(h.Reject))
	mux.HandleFunc("GET /api/supply-listings/{id}/media/{asset}", auth.Require(h.Media))
	srv := httptest.NewServer(mux)
	e.t.Cleanup(srv.Close)
	return httpClient{t: e.t, srv: srv}
}

func TestIntegration_Curation_HTTP(t *testing.T) {
	e := newListingEnv(t)
	_, l := e.draftFromWhatsApp("2348051120010", "HT")
	c := e.server(testOperatorToken)
	base := "/api/supply-listings/" + l.ID.String()

	// Unauthorized publication / edit / reject / preview (Test 13).
	for _, tc := range []struct{ method, path, token string }{
		{"POST", base + "/publish", ""}, {"POST", base + "/publish", "wrong-token-wrong-token"},
		{"POST", base + "/activate", ""}, {"POST", base + "/reject", ""}, {"PATCH", base, ""},
		{"GET", base + "/media/" + l.MediaReferences[0].AssetID, ""}, {"POST", "/api/supply-listings", ""},
		{"GET", base, ""}, {"GET", "/api/supply-listings?status=DRAFT", ""},
		{"GET", "/api/vendors/" + l.VendorID.String() + "/supply-listings", ""},
	} {
		code, body, _ := c.do(tc.method, tc.path, tc.token, map[string]any{})
		if code != http.StatusUnauthorized || body["code"] != "UNAUTHORIZED" {
			t.Fatalf("%s %s with %q: want 401, got %d %v", tc.method, tc.path, tc.token, code, body)
		}
	}
	if got, _ := e.listings.FindByID(context.Background(), l.ID); got.Status != supply.ListingDraft ||
		e.events(event.SupplyListingPublished, l.ID) != 0 {
		t.Fatal("an unauthorized call changed the listing")
	}

	// PATCH cannot set status or system-owned fields (§32-33).
	for _, field := range []string{"status", "vendor_id", "supply_signal_id", "public_reference", "published_at", "created_at", "media_references"} {
		code, body, _ := c.do("PATCH", base, testOperatorToken, map[string]any{field: "ACTIVE"})
		if code != http.StatusBadRequest {
			t.Fatalf("PATCH %s: want 400, got %d %v", field, code, body)
		}
	}
	if got, _ := e.listings.FindByID(context.Background(), l.ID); got.Status != supply.ListingDraft {
		t.Fatal("PATCH changed status")
	}

	// Detail.
	code, detail, _ := c.do("GET", base, testOperatorToken, nil)
	if code != 200 || detail["source_signal"] == nil || len(detail["source_messages"].([]any)) != 4 ||
		len(detail["media"].([]any)) != 3 || len(detail["publish_blockers"].([]any)) != 0 || detail["vendor"] == nil {
		t.Fatalf("detail: %d %v", code, detail)
	}
	updatedAt := detail["updated_at"].(string)

	// Validation errors are structured (Test 4 over HTTP).
	code, body, _ := c.do("PATCH", base, testOperatorToken, map[string]any{"price": -5, "product": " ", "currency": "USD", "quantity": 0})
	if code != 422 || body["code"] != "VALIDATION" || len(body["problems"].([]any)) != 4 {
		t.Fatalf("validation: %d %v", code, body)
	}

	// Edit with stale protection.
	code, edited, _ := c.do("PATCH", base, testOperatorToken, map[string]any{"product": "Apple iPhone 13 128GB", "expected_updated_at": updatedAt})
	if code != 200 || edited["product"] != "Apple iPhone 13 128GB" {
		t.Fatalf("edit: %d %v", code, edited)
	}
	code, body, _ = c.do("PATCH", base, testOperatorToken, map[string]any{"product": "Old tab", "expected_updated_at": updatedAt})
	if code != 409 || body["code"] != "STALE" {
		t.Fatalf("stale edit: %d %v", code, body)
	}
	code, body, _ = c.do("POST", base+"/publish", testOperatorToken, map[string]any{"expected_updated_at": updatedAt})
	if code != 409 || body["code"] != "STALE" {
		t.Fatalf("stale publish: %d %v", code, body)
	}

	// Media preview: this listing's asset -> bytes; anything else -> 404.
	code, _, raw := c.do("GET", base+"/media/"+l.MediaReferences[0].AssetID, testOperatorToken, nil)
	if code != 200 || !bytes.Equal(raw, photo1) {
		t.Fatalf("preview: %d (%d bytes)", code, len(raw))
	}
	code, body, _ = c.do("GET", base+"/media/"+uuid.NewString(), testOperatorToken, nil)
	if code != 404 || body["code"] != "NOT_FOUND" {
		t.Fatalf("foreign asset preview: %d %v", code, body)
	}
	// File gone from storage -> unavailable, not a hang or a 500.
	var key string
	_ = e.db.QueryRow(`SELECT storage_key FROM media_assets WHERE id = $1`, l.MediaReferences[2].AssetID).Scan(&key)
	if err := e.store.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	code, body, _ = c.do("GET", base+"/media/"+l.MediaReferences[2].AssetID, testOperatorToken, nil)
	if code != 404 || body["code"] != "MEDIA_UNAVAILABLE" {
		t.Fatalf("missing file: %d %v", code, body)
	}

	// Publish, then again (idempotent), then the legacy alias.
	code, pub, _ := c.do("POST", base+"/publish", testOperatorToken, nil)
	if code != 200 || pub["status"] != "ACTIVE" || pub["published_at"] == nil {
		t.Fatalf("publish: %d %v", code, pub)
	}
	for _, p := range []string{"/publish", "/activate"} {
		if code, again, _ := c.do("POST", base+p, testOperatorToken, nil); code != 200 || again["published_at"] != pub["published_at"] {
			t.Fatalf("re-publish via %s: %d %v", p, code, again)
		}
	}
	if e.events(event.SupplyListingPublished, l.ID) != 1 {
		t.Fatal("duplicate publication event")
	}

	// Errors never leak SQL.
	code, body, raw = c.do("POST", "/api/supply-listings/"+uuid.NewString()+"/publish", testOperatorToken, nil)
	if code != 404 || body["code"] != "NOT_FOUND" || bytes.Contains(raw, []byte("sql")) {
		t.Fatalf("unknown listing: %d %s", code, raw)
	}
	code, body, _ = c.do("POST", base+"/reject", testOperatorToken, map[string]any{"reason": "withdrawn", "status": "DRAFT"})
	if code != 400 {
		t.Fatalf("reject with unknown field: %d %v", code, body)
	}
	code, body, _ = c.do("POST", base+"/reject", testOperatorToken, map[string]any{"reason": "withdrawn"})
	if code != 200 || body["status"] != "ARCHIVED" || body["rejection_reason"] != "withdrawn" {
		t.Fatalf("reject: %d %v", code, body)
	}
	code, body, _ = c.do("PATCH", base, testOperatorToken, map[string]any{"product": "x"})
	if code != 409 || body["code"] != "INVALID_TRANSITION" {
		t.Fatalf("edit after reject: %d %v", code, body)
	}

	// Not publishable -> 422 with reasons, listing stays DRAFT.
	v2 := e.vendorAt("NP", "2348051120011", "")
	l2, _, _ := e.svc.CreateFromSignal(context.Background(), e.signal(v2, "Mystery", 1000, "", nil), event.ActorSystem, nil)
	mustExec(t, e.db, `UPDATE supply_listings SET price = NULL WHERE id = $1`, l2.ID)
	code, body, _ = c.do("POST", "/api/supply-listings/"+l2.ID.String()+"/publish", testOperatorToken, nil)
	if code != 422 || body["code"] != "NOT_PUBLISHABLE" {
		t.Fatalf("not publishable: %d %v", code, body)
	}
}

func TestIntegration_Curation_DevelopmentWithoutTokenIsOpen(t *testing.T) {
	e := newListingEnv(t)
	v := e.vendorAt("D", "2348051120012", "Benin City")
	l, _, _ := e.svc.CreateFromSignal(context.Background(), e.signal(v, "Fan", 30000, "", nil), event.ActorSystem, nil)
	c := e.server("")
	if code, body, _ := c.do("POST", "/api/supply-listings/"+l.ID.String()+"/publish", "", nil); code != 200 || body["status"] != "ACTIVE" {
		t.Fatalf("development (no token configured) keeps today's open boundary: %d %v", code, body)
	}
}

func i64(v int64) *int64 { return &v }

// Publication must not need a second pool connection while it holds the
// listing row lock: with a tiny pool, concurrent publishes of different
// listings must all complete (no connection-pool deadlock).
func TestIntegration_Curation_ConcurrentPublishesWithSmallPool(t *testing.T) {
	e := newListingEnv(t)
	ctx := context.Background()
	v := e.vendorAt("Pool", "2348051120013", "Benin City")
	var ids []uuid.UUID
	for i := 0; i < 8; i++ {
		l, _, err := e.svc.CreateFromSignal(ctx, e.signal(v, fmt.Sprint("Pool item ", i), 1000+int64(i), "", nil), event.ActorSystem, nil)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, l.ID)
	}
	e.db.SetMaxOpenConns(2)
	defer e.db.SetMaxOpenConns(0)
	tctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	errs := make(chan error, len(ids))
	for _, id := range ids {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			_, err := e.svc.Publish(tctx, id, nil, nil)
			errs <- err
		}(id)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("publish under a small pool: %v", err)
		}
	}
	if n := e.count(`SELECT count(*) FROM supply_listings WHERE vendor_id = $1 AND status = 'ACTIVE'`, v); n != len(ids) {
		t.Fatalf("want %d ACTIVE, got %d", len(ids), n)
	}
}
