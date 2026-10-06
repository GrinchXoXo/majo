package supply

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/vendor"
)

// curationFixture: one ACTIVE vendor, one signal, one DRAFT listing with two
// images and a video, all valid for publication.
type curationFixture struct {
	svc      *ListingService
	repo     *fakeListingRepo
	signals  *fakeSignalRepo
	vendors  fakeVendors
	media    fakeMedia
	vendorID uuid.UUID
	signalID uuid.UUID
	id       uuid.UUID
	img1     string
	img2     string
	vid      string
}

func newCurationFixture(t *testing.T) *curationFixture {
	t.Helper()
	f := &curationFixture{repo: newFakeListingRepo(), vendorID: uuid.New(), signalID: uuid.New(), id: uuid.New()}
	a1, a2, a3 := uuid.New(), uuid.New(), uuid.New()
	f.img1, f.img2, f.vid = a1.String(), a2.String(), a3.String()
	f.signals = &fakeSignalRepo{signals: map[uuid.UUID]*Signal{
		f.signalID: {ID: f.signalID, VendorID: f.vendorID, Product: strp("iPhone 13"), Price: i64p(620000)},
	}}
	f.vendors = fakeVendors{v: map[uuid.UUID]*vendor.Vendor{f.vendorID: {ID: f.vendorID, Status: vendor.StatusActive}}}
	f.media = fakeMedia{states: map[uuid.UUID]MediaAssetState{
		a1: {OwnerVendorID: f.vendorID, Kind: "image", Status: "AVAILABLE"},
		a2: {OwnerVendorID: f.vendorID, Kind: "image", Status: "AVAILABLE"},
		a3: {OwnerVendorID: f.vendorID, Kind: "video", Status: "AVAILABLE"},
	}}
	sig := f.signalID
	f.repo.listings[f.id] = &Listing{
		ID: f.id, VendorID: f.vendorID, SupplySignalID: &sig, PublicReference: "L-ABC123",
		Product: strp("iPhone 13"), Price: i64p(620000), Currency: "NGN", Location: strp("Benin City"),
		Status: ListingDraft, ExpiresAt: time.Now().Add(6 * 24 * time.Hour),
		MediaReferences: []MediaReference{{Type: "image", AssetID: f.img1}, {Type: "image", AssetID: f.img2}, {Type: "video", AssetID: f.vid}},
		CreatedAt:       time.Now().Add(-time.Hour), UpdatedAt: time.Now().Add(-time.Hour).Truncate(time.Microsecond),
	}
	f.rebuild()
	return f
}

func (f *curationFixture) rebuild() {
	f.svc = NewListingService(f.signals, f.repo, f.vendors, f.media, &fakeEventsForListing{}, &fakeAuditsForListing{}, testLogger())
}

func (f *curationFixture) get() *Listing { cp := *f.repo.listings[f.id]; return &cp }

func (f *curationFixture) eventsOf(t event.Type) int {
	n := 0
	for _, e := range f.repo.curationEvents {
		if e.EventType == t {
			n++
		}
	}
	return n
}

func ip(i int) *int { return &i }

// --- Test 3: edit ---

func TestCuration_EditPersistsAndIsAudited(t *testing.T) {
	f := newCurationFixture(t)
	before := f.get()
	got, err := f.svc.Edit(context.Background(), f.id, ListingEdit{
		Product: strp("  Apple iPhone 13 128GB "), Price: i64p(600000), Location: strp("GRA, Benin City"),
		Condition: strp("Brand new"), Currency: strp("ngn"), Quantity: ip(2),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if *got.Product != "Apple iPhone 13 128GB" || *got.Price != 600000 || *got.Location != "GRA, Benin City" ||
		*got.Condition != "Brand new" || got.Currency != "NGN" || *got.Quantity != 2 {
		t.Fatalf("edit not persisted: %+v", got)
	}
	if *got.NormalizedProduct != "appleiphone13128gb" {
		t.Fatalf("normalized product not recomputed: %q", *got.NormalizedProduct)
	}
	if got.Status != ListingDraft || got.PublishedAt != nil {
		t.Fatalf("an edit must never publish: %s %v", got.Status, got.PublishedAt)
	}
	if got.VendorID != before.VendorID || *got.SupplySignalID != *before.SupplySignalID || got.PublicReference != before.PublicReference {
		t.Fatal("system-owned fields changed")
	}
	if len(f.repo.curationAudits) != 1 || f.repo.curationAudits[0].Action != "SUPPLY_LISTING_CORRECTED" {
		t.Fatalf("want one SUPPLY_LISTING_CORRECTED audit, got %+v", f.repo.curationAudits)
	}
	a := f.repo.curationAudits[0]
	if a.PreviousValue["product"] != "iPhone 13" || a.NewValue["product"] != "Apple iPhone 13 128GB" ||
		a.PreviousValue["price"] != int64(620000) || a.NewValue["price"] != int64(600000) || a.ActorType != "OPERATOR" {
		t.Fatalf("audit lacks old/new values: %+v", a)
	}
	if _, ok := a.NewValue["currency"]; ok {
		t.Fatal("unchanged currency (ngn == NGN) must not be audited as a change")
	}
	if f.eventsOf(event.SupplyListingUpdated) != 1 || f.eventsOf(event.SupplyListingPublished) != 0 {
		t.Fatalf("an edit emits SUPPLY_LISTING_UPDATED only: %+v", f.repo.curationEvents)
	}
	// Source evidence (Test 5, unit level): the signal is untouched.
	if *f.signals.signals[f.signalID].Product != "iPhone 13" || *f.signals.signals[f.signalID].Price != 620000 {
		t.Fatal("source signal mutated")
	}
}

func TestCuration_NoOpEditWritesNothing(t *testing.T) {
	f := newCurationFixture(t)
	before := f.get()
	got, err := f.svc.Edit(context.Background(), f.id, ListingEdit{Product: strp("iPhone 13"), Price: i64p(620000)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !got.UpdatedAt.Equal(before.UpdatedAt) || len(f.repo.curationAudits) != 0 || len(f.repo.curationEvents) != 0 {
		t.Fatal("a no-op edit must not write")
	}
}

// --- Test 4: invalid edits ---

func TestCuration_InvalidEditsAreRejected(t *testing.T) {
	cases := map[string]struct {
		edit  ListingEdit
		field string
	}{
		"negative price":    {ListingEdit{Price: i64p(-1)}, "price"},
		"zero price":        {ListingEdit{Price: i64p(0)}, "price"},
		"empty product":     {ListingEdit{Product: strp("   ")}, "product"},
		"invalid currency":  {ListingEdit{Currency: strp("USD")}, "currency"},
		"made-up currency":  {ListingEdit{Currency: strp("XYZ")}, "currency"},
		"zero quantity":     {ListingEdit{Quantity: ip(0)}, "quantity"},
		"negative quantity": {ListingEdit{Quantity: ip(-3)}, "quantity"},
		"blank location":    {ListingEdit{Location: strp(" ")}, "location"},
		"too long product":  {ListingEdit{Product: strp(string(make([]byte, 201)))}, "product"},
		"unknown primary":   {ListingEdit{PrimaryAssetID: strp(uuid.NewString())}, "primary_media_asset_id"},
		"order adds media":  {ListingEdit{MediaOrder: []string{uuid.NewString()}}, "media_order"},
		"order drops media": {ListingEdit{MediaOrder: []string{}}, "media_order"},
		"order duplicates":  {ListingEdit{}, "media_order"}, // filled below
		"video as primary":  {ListingEdit{}, "primary_media_asset_id"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCurationFixture(t)
			e := c.edit
			switch name {
			case "order duplicates":
				e.MediaOrder = []string{f.img1, f.img1, f.vid}
			case "video as primary":
				e.PrimaryAssetID = &f.vid
			}
			// One valid change alongside: nothing at all may be applied.
			e.Condition = strp("used")
			before := f.get()
			_, err := f.svc.Edit(context.Background(), f.id, e, nil)
			var verr ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("want ValidationError, got %v", err)
			}
			found := false
			for _, p := range verr.Problems {
				found = found || p.Field == c.field
			}
			if !found {
				t.Fatalf("want a %s problem, got %+v", c.field, verr.Problems)
			}
			after := f.get()
			if after.Condition != before.Condition || len(f.repo.curationAudits) != 0 || len(f.repo.curationEvents) != 0 {
				t.Fatal("an invalid edit must change nothing")
			}
		})
	}
}

func TestCuration_EditOnlyDraftOrActive(t *testing.T) {
	for _, st := range []ListingStatus{ListingArchived, ListingPaused, ListingSold, ListingExpired, ListingUnavailable} {
		f := newCurationFixture(t)
		f.repo.listings[f.id].Status = st
		_, err := f.svc.Edit(context.Background(), f.id, ListingEdit{Product: strp("x y")}, nil)
		var terr ErrInvalidTransition
		if !errors.As(err, &terr) {
			t.Fatalf("%s: want ErrInvalidTransition, got %v", st, err)
		}
	}
}

func TestCuration_EditActiveKeepsPublication(t *testing.T) {
	f := newCurationFixture(t)
	if _, err := f.svc.Publish(context.Background(), f.id, nil, nil); err != nil {
		t.Fatal(err)
	}
	pub := *f.get().PublishedAt
	got, err := f.svc.Edit(context.Background(), f.id, ListingEdit{Price: i64p(610000)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ListingActive || !got.PublishedAt.Equal(pub) || got.ID != f.id {
		t.Fatal("editing an ACTIVE listing must keep the same listing, status and published_at")
	}
	if f.eventsOf(event.SupplyListingPublished) != 1 {
		t.Fatal("an edit of an ACTIVE listing must not re-emit SUPPLY_LISTING_PUBLISHED")
	}
}

// --- stale-tab protection ---

func TestCuration_StaleEditAndPublishAreRefused(t *testing.T) {
	f := newCurationFixture(t)
	old := f.get().UpdatedAt
	if _, err := f.svc.Edit(context.Background(), f.id, ListingEdit{Product: strp("A"), ExpectedUpdatedAt: &old}, nil); err != nil {
		t.Fatal(err)
	}
	// The other tab still holds `old`.
	if _, err := f.svc.Edit(context.Background(), f.id, ListingEdit{Product: strp("B"), ExpectedUpdatedAt: &old}, nil); !errors.Is(err, ErrStale) {
		t.Fatalf("stale edit: want ErrStale, got %v", err)
	}
	if _, err := f.svc.Publish(context.Background(), f.id, &old, nil); !errors.Is(err, ErrStale) {
		t.Fatalf("stale publish: want ErrStale, got %v", err)
	}
	if _, err := f.svc.Reject(context.Background(), f.id, nil, &old, nil); !errors.Is(err, ErrStale) {
		t.Fatalf("stale reject: want ErrStale, got %v", err)
	}
	if got := f.get(); *got.Product != "A" || got.Status != ListingDraft {
		t.Fatalf("stale operations must not apply: %+v", got)
	}
}

// --- Test 7: primary media ---

func TestCuration_PrimaryMediaAndOrder(t *testing.T) {
	f := newCurationFixture(t)
	if p := PrimaryMedia(f.get().MediaReferences); p.AssetID != f.img1 {
		t.Fatal("default primary must be the first image")
	}
	got, err := f.svc.Edit(context.Background(), f.id, ListingEdit{
		PrimaryAssetID: &f.img2, MediaOrder: []string{f.vid, f.img2, f.img1},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs := got.MediaReferences
	if len(refs) != 3 || refs[0].AssetID != f.vid || refs[1].AssetID != f.img2 || refs[2].AssetID != f.img1 {
		t.Fatalf("order not saved: %+v", refs)
	}
	if p := PrimaryMedia(refs); p == nil || p.AssetID != f.img2 || refs[1].Role != MediaRolePrimary {
		t.Fatalf("primary not saved: %+v", refs)
	}
	// Changing primary moves the role; exactly one PRIMARY.
	got, _ = f.svc.Edit(context.Background(), f.id, ListingEdit{PrimaryAssetID: &f.img1}, nil)
	n := 0
	for _, m := range got.MediaReferences {
		if m.Role == MediaRolePrimary {
			n++
		}
	}
	if n != 1 || PrimaryMedia(got.MediaReferences).AssetID != f.img1 {
		t.Fatalf("exactly one primary expected: %+v", got.MediaReferences)
	}
	if _, ok := f.repo.curationAudits[0].NewValue["media"]; !ok {
		t.Fatal("media change must be audited")
	}
}

// --- Tests 8, 10, 14, 16: publish ---

func TestCuration_PublishDraft(t *testing.T) {
	f := newCurationFixture(t)
	got, err := f.svc.Publish(context.Background(), f.id, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ListingActive || got.PublishedAt == nil {
		t.Fatalf("want ACTIVE with published_at: %s %v", got.Status, got.PublishedAt)
	}
	if f.eventsOf(event.SupplyListingPublished) != 1 {
		t.Fatalf("want exactly one SUPPLY_LISTING_PUBLISHED, got %+v", f.repo.curationEvents)
	}
	ev := f.repo.curationEvents[0]
	if ev.Payload["public_reference"] != "L-ABC123" || ev.Payload["primary_media_asset_id"] != f.img1 || ev.ActorType != event.ActorOperator {
		t.Fatalf("event payload: %+v", ev.Payload)
	}
	a := f.repo.curationAudits[0]
	if a.Action != "SUPPLY_LISTING_PUBLISHED" || a.PreviousValue["status"] != "DRAFT" || a.NewValue["status"] != "ACTIVE" {
		t.Fatalf("audit: %+v", a)
	}
}

func TestCuration_PublishIsIdempotent(t *testing.T) {
	f := newCurationFixture(t)
	first, _ := f.svc.Publish(context.Background(), f.id, nil, nil)
	stale := time.Now().Add(-48 * time.Hour)
	for i := 0; i < 3; i++ {
		again, err := f.svc.Publish(context.Background(), f.id, &stale, nil)
		if err != nil {
			t.Fatalf("re-publishing an ACTIVE listing must be a no-op, got %v", err)
		}
		if !again.PublishedAt.Equal(*first.PublishedAt) || !again.UpdatedAt.Equal(first.UpdatedAt) {
			t.Fatal("re-publish changed timestamps")
		}
	}
	if f.eventsOf(event.SupplyListingPublished) != 1 || len(f.repo.curationAudits) != 1 {
		t.Fatal("ACTIVE -> ACTIVE must not re-run publication")
	}
}

func TestCuration_PublishFromOtherStatusesFails(t *testing.T) {
	for _, st := range []ListingStatus{ListingArchived, ListingPaused, ListingSold, ListingExpired, ListingUnavailable} {
		f := newCurationFixture(t)
		f.repo.listings[f.id].Status = st
		_, err := f.svc.Publish(context.Background(), f.id, nil, nil)
		var terr ErrInvalidTransition
		if !errors.As(err, &terr) || f.get().Status != st || len(f.repo.curationEvents) != 0 {
			t.Fatalf("%s: want refused transition, got %v", st, err)
		}
	}
}

// --- Test 9 / Example 67: invalid drafts stay DRAFT ---

func TestCuration_PublishInvalidDraftStaysDraft(t *testing.T) {
	cases := map[string]struct {
		mutate func(f *curationFixture)
		field  string
	}{
		"no product":        {func(f *curationFixture) { f.repo.listings[f.id].Product = nil }, "product"},
		"blank product":     {func(f *curationFixture) { f.repo.listings[f.id].Product = strp(" ") }, "product"},
		"no price":          {func(f *curationFixture) { f.repo.listings[f.id].Price = nil }, "price"},
		"vendor blocked":    {func(f *curationFixture) { f.vendors.v[f.vendorID].Status = vendor.StatusBlocked }, "vendor"},
		"vendor inactive":   {func(f *curationFixture) { f.vendors.v[f.vendorID].Status = vendor.StatusInactive }, "vendor"},
		"vendor missing":    {func(f *curationFixture) { delete(f.vendors.v, f.vendorID) }, "vendor"},
		"no source signal":  {func(f *curationFixture) { f.repo.listings[f.id].SupplySignalID = nil }, "supply_signal_id"},
		"signal deleted":    {func(f *curationFixture) { delete(f.signals.signals, f.signalID) }, "supply_signal_id"},
		"signal of another": {func(f *curationFixture) { f.signals.signals[f.signalID].VendorID = uuid.New() }, "supply_signal_id"},
		"expired":           {func(f *curationFixture) { f.repo.listings[f.id].ExpiresAt = time.Now().Add(-time.Minute) }, "expires_at"},
		"bad currency":      {func(f *curationFixture) { f.repo.listings[f.id].Currency = "USD" }, "currency"},
		"missing asset": {func(f *curationFixture) {
			f.repo.listings[f.id].MediaReferences = append(f.repo.listings[f.id].MediaReferences, MediaReference{Type: "image", AssetID: uuid.NewString()})
		}, "media"},
		"foreign asset": {func(f *curationFixture) {
			id := uuid.MustParse(f.img2)
			st := f.media.states[id]
			st.OwnerVendorID = uuid.New()
			f.media.states[id] = st
		}, "media"},
		"primary removed": {func(f *curationFixture) {
			id := uuid.MustParse(f.img1)
			st := f.media.states[id]
			st.Status = "REMOVED"
			f.media.states[id] = st
		}, "primary_media_asset_id"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCurationFixture(t)
			c.mutate(f)
			f.rebuild()
			_, err := f.svc.Publish(context.Background(), f.id, nil, nil)
			var perr ErrNotPublishable
			if !errors.As(err, &perr) {
				t.Fatalf("want ErrNotPublishable, got %v", err)
			}
			found := false
			for _, r := range perr.Reasons {
				found = found || r.Field == c.field
			}
			if !found {
				t.Fatalf("want a %s reason, got %+v", c.field, perr.Reasons)
			}
			if got := f.get(); got.Status != ListingDraft || got.PublishedAt != nil {
				t.Fatal("a failed publish must leave the listing DRAFT")
			}
			if len(f.repo.curationEvents) != 0 || len(f.repo.curationAudits) != 0 {
				t.Fatal("a failed publish must emit nothing")
			}
		})
	}
}

func TestCuration_MediaIsOptionalForPublication(t *testing.T) {
	f := newCurationFixture(t)
	f.repo.listings[f.id].MediaReferences = []MediaReference{}
	if _, err := f.svc.Publish(context.Background(), f.id, nil, nil); err != nil {
		t.Fatalf("media is optional in the current model: %v", err)
	}
}

// --- Tests 11, 12: reject ---

func TestCuration_RejectDraftKeepsEvidence(t *testing.T) {
	f := newCurationFixture(t)
	got, err := f.svc.Reject(context.Background(), f.id, strp("  product unclear "), nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != ListingArchived || got.RejectedAt == nil || *got.RejectionReason != "product unclear" {
		t.Fatalf("want ARCHIVED + reason: %+v", got)
	}
	if len(got.MediaReferences) != 3 || f.signals.signals[f.signalID] == nil {
		t.Fatal("rejection must keep media references and the signal")
	}
	if f.eventsOf(event.SupplyListingRejected) != 1 || f.repo.curationAudits[0].Action != "SUPPLY_LISTING_REJECTED" {
		t.Fatal("want one SUPPLY_LISTING_REJECTED event + audit")
	}
	// Idempotent.
	again, err := f.svc.Reject(context.Background(), f.id, strp("other"), nil, nil)
	if err != nil || *again.RejectionReason != "product unclear" || !again.RejectedAt.Equal(*got.RejectedAt) {
		t.Fatalf("re-reject must be a no-op: %v %+v", err, again)
	}
	if f.eventsOf(event.SupplyListingRejected) != 1 {
		t.Fatal("re-reject re-emitted")
	}
	// No republish route.
	if _, err := f.svc.Publish(context.Background(), f.id, nil, nil); !errors.As(err, new(ErrInvalidTransition)) {
		t.Fatalf("a rejected listing cannot be published: %v", err)
	}
	if _, err := f.svc.Edit(context.Background(), f.id, ListingEdit{Product: strp("x")}, nil); !errors.As(err, new(ErrInvalidTransition)) {
		t.Fatalf("a rejected listing cannot be edited: %v", err)
	}
}

func TestCuration_RejectWithoutReasonAndFromActive(t *testing.T) {
	f := newCurationFixture(t)
	f.svc.Publish(context.Background(), f.id, nil, nil)
	got, err := f.svc.Reject(context.Background(), f.id, strp("   "), nil, nil)
	if err != nil || got.Status != ListingArchived || got.RejectionReason != nil || got.PublishedAt == nil {
		t.Fatalf("withdrawing an ACTIVE listing: %v %+v", err, got)
	}
	ev := f.repo.curationEvents[len(f.repo.curationEvents)-1]
	if ev.EventType != event.SupplyListingRejected || ev.Payload["from_status"] != "ACTIVE" {
		t.Fatalf("event: %+v", ev)
	}
}

func TestCuration_RejectInvalidStatusesAndReason(t *testing.T) {
	f := newCurationFixture(t)
	f.repo.listings[f.id].Status = ListingSold
	if _, err := f.svc.Reject(context.Background(), f.id, nil, nil, nil); !errors.As(err, new(ErrInvalidTransition)) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
	f = newCurationFixture(t)
	long := string(make([]byte, 501))
	if _, err := f.svc.Reject(context.Background(), f.id, &long, nil, nil); !errors.As(err, new(ValidationError)) {
		t.Fatalf("want ValidationError for a too-long reason, got %v", err)
	}
}

func TestCuration_UnknownListing(t *testing.T) {
	f := newCurationFixture(t)
	for _, err := range []error{
		func() error { _, e := f.svc.Publish(context.Background(), uuid.New(), nil, nil); return e }(),
		func() error { _, e := f.svc.Reject(context.Background(), uuid.New(), nil, nil, nil); return e }(),
		func() error {
			_, e := f.svc.Edit(context.Background(), uuid.New(), ListingEdit{Product: strp("x")}, nil)
			return e
		}(),
	} {
		if !errors.Is(err, ErrListingNotFound) {
			t.Fatalf("want ErrListingNotFound, got %v", err)
		}
	}
}

// Test 16 (second half): creating a draft never emits publication.
func TestCuration_DraftCreationDoesNotPublish(t *testing.T) {
	signals := &fakeSignalRepo{signals: map[uuid.UUID]*Signal{}}
	repo := newFakeListingRepo()
	events := &fakeEventsForListing{}
	vid := uuid.New()
	sid := uuid.New()
	signals.signals[sid] = fresh(&Signal{ID: sid, VendorID: vid, Product: strp("Bag"), Price: i64p(18500)})
	svc := newSvc(signals, repo, events, &fakeAuditsForListing{}, activeVendor(vid))
	l, err := svc.PublishFromSignal(context.Background(), sid, nil)
	if err != nil {
		t.Fatal(err)
	}
	if l.Status != ListingDraft || l.PublishedAt != nil {
		t.Fatalf("draft creation must stay DRAFT: %+v", l)
	}
	for _, e := range append(events.events, repo.curationEvents...) {
		if e.EventType == event.SupplyListingPublished {
			t.Fatal("draft creation emitted SUPPLY_LISTING_PUBLISHED")
		}
	}
}
