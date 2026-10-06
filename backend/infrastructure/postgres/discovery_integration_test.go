package postgres

// Phase 4B Postgres integration tests: consent, dry run, batch send through
// the recording sender, caps, dedupe, staff-only gate, structured
// interactions, the discovery-origin commerce loop with listing provenance,
// partial failure, expired supply, attribution and the funnel.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/channel"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/discovery"
	"github.com/middleman/backend/internal/distribution"
	"github.com/middleman/backend/internal/offer"
	"github.com/middleman/backend/internal/presentation"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/structid"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/transaction"
)

// fakeChannel is the channel provider: records every send, can fail per
// recipient, and serves media uploads.
type fakeChannel struct {
	mu         sync.Mutex
	sent       []sentMessage
	failTo     map[string]bool
	uploads    int
	failUpload bool
}

type sentMessage struct {
	to  string
	msg channel.Message
}

func (f *fakeChannel) SendTextWithID(ctx context.Context, to, body string) (string, error) {
	return f.SendMessage(ctx, to, channel.Message{Kind: channel.KindText, Body: body})
}

func (f *fakeChannel) SendMessage(ctx context.Context, to string, m channel.Message) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failTo[to] {
		return "", errors.New("whatsapp api returned status 400: (#131047) Re-engagement message")
	}
	f.sent = append(f.sent, sentMessage{to, m})
	return "wamid.test." + uuid.NewString(), nil
}

func (f *fakeChannel) UploadMedia(ctx context.Context, data []byte, mime, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failUpload {
		return "", errors.New("upload failed")
	}
	f.uploads++
	return fmt.Sprintf("upl-%d", f.uploads), nil
}

func (f *fakeChannel) to(r string) []channel.Message {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []channel.Message
	for _, s := range f.sent {
		if s.to == r {
			out = append(out, s.msg)
		}
	}
	return out
}

type memBlobs struct{}

func (memBlobs) Get(ctx context.Context, key string) ([]byte, error) {
	return []byte("IMG:" + key), nil
}

type discoveryEnv struct {
	*listingEnv
	ch       *fakeChannel
	repo     *DiscoveryRepository
	disc     *discovery.Service
	dist     *distribution.Service
	reqSvc   *request.Service
	offerSvc *offer.Service
	txSvc    *transaction.Service
	cfg      distribution.Config
}

const staffA, staffB, staffC, outsider = "2348100000001", "2348100000002", "2348100000003", "2348199999999"

func newDiscoveryEnv(t *testing.T, mod func(*distribution.Config)) *discoveryEnv {
	t.Helper()
	le := newListingEnv(t)
	e := &discoveryEnv{listingEnv: le, ch: &fakeChannel{failTo: map[string]bool{}}, repo: NewDiscoveryRepository(le.db)}
	conv := NewConversationRepository(le.db)
	out := conversation.NewRecordingSender(e.ch, conv, "whatsapp", discard())
	reqRepo := NewRequestRepository(le.db)
	e.reqSvc = request.NewService(reqRepo, conv, testExtractor{}, NewEventWriter(le.db), NewAuditWriter(le.db), out, discard())
	e.routing.SetListingEvidence(le.listings, nil)
	e.routing.SetListingOrigin(le.listings, le.svc)
	e.offerSvc = offer.NewService(NewOfferRepository(le.db), NewRoutingRepository(le.db), reqRepo, NewEventWriter(le.db), NewAuditWriter(le.db), discard())
	e.txSvc = transaction.NewService(NewTransactionRepository(le.db), NewOfferRepository(le.db), reqRepo, NewEventWriter(le.db), NewAuditWriter(le.db), discard())
	e.disc = discovery.NewService(discovery.NewEngine(nil), e.repo)
	win, _ := distribution.ParseWindow("", "Africa/Lagos")
	e.cfg = distribution.Config{
		Mode: distribution.ModeInternal, InternalRecipients: map[string]bool{staffA: true, staffB: true, staffC: true},
		Arms: []presentation.Arm{presentation.ArmA, presentation.ArmB}, MaxDeliveriesPerBuyer: 1, CapPeriod: 72 * time.Hour,
		ExposureWindow: 14 * 24 * time.Hour, ListingsPerDelivery: 3, Window: win, InteractionTTL: 7 * 24 * time.Hour,
		NegativeWindow: 30 * 24 * time.Hour, ConsentWording: "v1", EnquiryConfirmation: distribution.ConfirmByOperator,
		Ranking: discovery.DefaultParams(), OutboundMediaTTL: 20 * 24 * time.Hour,
	}
	if mod != nil {
		mod(&e.cfg)
	}
	if err := e.cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	pub := distribution.NewMediaPublisher(NewMediaAssetRepository(le.db), memBlobs{}, e.ch, e.cfg.OutboundMediaTTL)
	e.dist = distribution.NewService(e.cfg, e.repo, e.disc, out, pub, e.reqSvc, NewEventWriter(le.db), discard())
	return e
}

// buyer creates a buyer + conversation with one recent inbound message
// (inside the 24h window) and returns its ID.
func (e *discoveryEnv) buyer(phone string) uuid.UUID {
	e.t.Helper()
	id, conv := uuid.New(), uuid.New()
	mustExec(e.t, e.db, `INSERT INTO buyers (id, external_user_id) VALUES ($1, $2)`, id, phone)
	mustExec(e.t, e.db, `INSERT INTO conversations (id, external_id, buyer_id) VALUES ($1, $2, $3)`, conv, phone, id)
	e.inbound(phone, "hi")
	return id
}

// inbound records an inbound message from phone and returns its ID.
func (e *discoveryEnv) inbound(phone, text string) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	mustExec(e.t, e.db, `INSERT INTO messages (id, conversation_id, direction, external_message_id, message_type, content)
		SELECT $1, c.id, 'INBOUND', $2, 'text', jsonb_build_object('text', $3::text) FROM conversations c WHERE c.external_id = $4`,
		id, "wamid.in."+id.String(), text, phone)
	return id
}

func (e *discoveryEnv) optIn(phone, location string, interests ...string) {
	e.t.Helper()
	st := distribution.OptedIn
	if _, err := e.dist.SetStaffPreference(context.Background(), distribution.StaffPreference{
		Recipient: phone, Status: &st, Location: &location, Interests: interests, OperatorID: "test"}); err != nil {
		e.t.Fatal(err)
	}
}

// image attaches a stored image asset to a listing.
func (e *discoveryEnv) image(l *supply.Listing, kind string) uuid.UUID {
	e.t.Helper()
	msg, asset := uuid.New(), uuid.New()
	mustExec(e.t, e.db, `INSERT INTO conversations (id, external_id) VALUES (gen_random_uuid(), $1) ON CONFLICT DO NOTHING`, "v-media-"+l.ID.String())
	mustExec(e.t, e.db, `INSERT INTO messages (id, conversation_id, direction, external_message_id, message_type, content)
		SELECT $1, c.id, 'INBOUND', $2, $3, '{}'::jsonb FROM conversations c WHERE c.external_id = $4`, msg, "wamid.m."+msg.String(), kind, "v-media-"+l.ID.String())
	mime := "image/png"
	if kind == "video" {
		mime = "video/mp4"
	}
	mustExec(e.t, e.db, `INSERT INTO media_assets (id, owner_vendor_id, kind, mime_type, size_bytes, sha256, storage_key, source_message_id, provider, provider_media_id)
		VALUES ($1, $2, $3, $4, 10, $5, $6, $7, 'whatsapp', 'pm')`, asset, l.VendorID, kind, mime,
		strings.Repeat("a", 56)+asset.String()[:8], "k/"+asset.String(), msg)
	mustExec(e.t, e.db, `UPDATE supply_listings SET media_references = media_references || jsonb_build_array(jsonb_build_object('type', $2::text, 'asset_id', $3::text))
		WHERE id = $1`, l.ID, kind, asset.String())
	return asset
}

func (e *discoveryEnv) listing(vendorLocation, product, category string, price int64) *supply.Listing {
	e.t.Helper()
	v := e.vendorAt("V "+product, "2348077"+fmt.Sprint(time.Now().UnixNano())[10:], vendorLocation)
	l := e.publishedListing(v, product, price)
	mustExec(e.t, e.db, `UPDATE supply_listings SET category = $2, condition = 'New', location = $3 WHERE id = $1`, l.ID, category, vendorLocation)
	out, _ := e.listings.FindByID(context.Background(), l.ID)
	return out
}

func (e *discoveryEnv) batch(key string, recipients ...string) (*distribution.Batch, []*distribution.Delivery) {
	e.t.Helper()
	ctx := context.Background()
	b, _, err := e.dist.CreateBatch(ctx, distribution.PlanRequest{ClientKey: key, Recipients: recipients})
	if err != nil {
		e.t.Fatal(err)
	}
	_, ds, err := e.dist.Batch(ctx, b.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	return b, ds
}

func deliveryFor(ds []*distribution.Delivery, recipient string) *distribution.Delivery {
	for _, d := range ds {
		if d.Recipient == recipient {
			return d
		}
	}
	return nil
}

func (e *discoveryEnv) tap(buyerID uuid.UUID, phone, raw string) uuid.UUID {
	msg := e.inbound(phone, raw)
	conv := uuid.Nil
	_ = e.db.QueryRow(`SELECT id FROM conversations WHERE external_id = $1`, phone).Scan(&conv)
	e.dist.HandleStructured(context.Background(), distribution.Inbound{BuyerID: buyerID, ConversationID: conv, MessageID: msg,
		ReplyTo: phone, RawID: raw, Tapped: true})
	return msg
}

// commerceState is everything a stray interaction must never change.
func (e *discoveryEnv) commerceState() string {
	return fmt.Sprint(e.count(`SELECT count(*) FROM buyer_requests`), e.count(`SELECT count(*) FROM offers`),
		e.count(`SELECT count(*) FROM buyer_selections`), e.count(`SELECT count(*) FROM transactions`),
		e.count(`SELECT count(*) FROM discovery_preference_history`), e.count(`SELECT count(*) FROM discovery_preferences WHERE status = 'OPTED_IN'`))
}

func TestIntegration_Discovery_ConsentIsSeparateExplicitAndAudited(t *testing.T) {
	e := newDiscoveryEnv(t, nil)
	ctx := context.Background()
	a := e.buyer(staffA)
	e.buyer(outsider)
	// No hidden default: buyers exist, nobody is opted in.
	if e.count(`SELECT count(*) FROM discovery_preferences`) != 0 {
		t.Fatal("no consent record may exist without an explicit opt-in")
	}
	st := distribution.OptedIn
	loc := "GRA, Benin City"
	if _, err := e.dist.SetStaffPreference(ctx, distribution.StaffPreference{Recipient: outsider, Status: &st, Location: &loc}); !errors.Is(err, distribution.ErrNotPermitted) {
		t.Fatalf("a non-staff number can never get a staff consent record: %v", err)
	}
	if _, err := e.dist.SetStaffPreference(ctx, distribution.StaffPreference{Recipient: "2348100000009", Status: &st}); !errors.Is(err, distribution.ErrNotPermitted) {
		t.Fatalf("unknown number: %v", err)
	}
	e.optIn(staffA, loc, "electronics")
	p, _ := e.repo.Preference(ctx, a)
	if p == nil || p.Status != distribution.OptedIn || *p.Location != loc || p.Source != distribution.SourceOperatorStaff {
		t.Fatalf("opt-in record: %+v", p)
	}
	// Consent is not on Buyer and not on BuyerRequest.
	if e.count(`SELECT count(*) FROM information_schema.columns WHERE table_name IN ('buyers', 'buyer_requests') AND column_name ILIKE '%consent%'`) != 0 {
		t.Fatal("consent must not be a buyer/request column")
	}
	// Re-applying the same state is a no-op (no history, no event).
	e.optIn(staffA, loc, "electronics")
	if e.count(`SELECT count(*) FROM discovery_preference_history WHERE buyer_id = $1`, a) != 1 ||
		e.count(`SELECT count(*) FROM events WHERE event_type = 'DISCOVERY_OPTED_IN' AND entity_id = $1`, a) != 1 {
		t.Fatal("idempotent opt-in")
	}
	// STOP keyword: opt-out, history kept, no BuyerRequest.
	msg := e.inbound(staffA, "STOP")
	var conv uuid.UUID
	_ = e.db.QueryRow(`SELECT id FROM conversations WHERE external_id = $1`, staffA).Scan(&conv)
	e.dist.HandleStopKeyword(ctx, distribution.Inbound{BuyerID: a, ConversationID: conv, MessageID: msg, ReplyTo: staffA})
	p, _ = e.repo.Preference(ctx, a)
	if p.Status != distribution.OptedOut || p.OptedOutAt == nil || p.Source != distribution.SourceKeyword || p.SourceMessageID == nil || *p.SourceMessageID != msg {
		t.Fatalf("opt-out: %+v", p)
	}
	if e.count(`SELECT count(*) FROM discovery_preference_history WHERE buyer_id = $1`, a) != 2 || e.count(`SELECT count(*) FROM buyer_requests`) != 0 {
		t.Fatal("history is append-only; STOP never becomes a request")
	}
	// The dry run excludes the opted-out buyer.
	rep, err := e.dist.Preview(ctx, distribution.PlanRequest{})
	if err != nil || rep.ExcludedBuyers[distribution.ReasonOptedOut] != 1 || rep.Deliveries != 0 {
		t.Fatalf("opted-out excluded: %+v %v", rep.ExcludedBuyers, err)
	}
}

func TestIntegration_Discovery_DryRunSendCapsDedupeAndGate(t *testing.T) {
	e := newDiscoveryEnv(t, nil)
	ctx := context.Background()
	tv := e.listing("GRA, Benin City", "Samsung TV 55", "electronics", 450000)
	e.image(tv, "image")
	bag := e.listing("GRA, Benin City", "Ladies bag", "fashion", 18500)
	a, b := e.buyer(staffA), e.buyer(staffB)
	e.buyer(outsider)
	e.optIn(staffA, "GRA, Benin City", "electronics")
	e.optIn(staffB, "GRA, Benin City")
	// An outsider with a (pre-existing, e.g. imported) opt-in is still blocked by the gate.
	mustExec(t, e.db, `INSERT INTO discovery_preferences (buyer_id, status, wording_version, source, captured_at)
		SELECT id, 'OPTED_IN', 'v1', 'operator_staff', now() FROM buyers WHERE external_user_id = $1`, outsider)
	e.buyer("2348100000003") // staff number, never opted in

	// Dry run: audience, ranking, exclusions, nothing sent or stored.
	rep, err := e.dist.Preview(ctx, distribution.PlanRequest{Recipients: []string{staffA, staffB, staffC, outsider, "2348100000077"}})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Deliveries != 2 || rep.AudienceEligible != 2 || rep.ExcludedBuyers[distribution.ReasonNotInternalRecipient] != 1 ||
		rep.ExcludedBuyers[distribution.ReasonNoConsent] != 1 || rep.ExcludedBuyers[distribution.ReasonUnknownBuyer] != 1 || rep.EligibleListings != 2 {
		t.Fatalf("dry run: %+v", rep)
	}
	var planA *distribution.BuyerPlan
	for i := range rep.Buyers {
		if rep.Buyers[i].Recipient == staffA {
			planA = &rep.Buyers[i]
		}
	}
	if planA == nil || planA.Candidates[0].PublicReference != tv.PublicReference || len(planA.Candidates[0].Reasons) == 0 ||
		!planA.Candidates[0].Eligibility.ViewerEligible || planA.Candidates[0].Eligibility.GeoStage != "LOCAL" {
		t.Fatalf("ranking + canonical eligibility in the dry run: %+v", planA)
	}
	if len(e.ch.sent) != 0 || e.count(`SELECT count(*) FROM discovery_batches`) != 0 || e.count(`SELECT count(*) FROM messages WHERE direction = 'OUTBOUND'`) != 0 {
		t.Fatal("a dry run sends and stores nothing")
	}

	// A batch is PLANNED; creating it sends nothing; creation is idempotent.
	b1, ds := e.batch("b1")
	if b1.Status != distribution.BatchPlanned || len(ds) != 2 || len(e.ch.sent) != 0 {
		t.Fatalf("planned batch: %s %d %d", b1.Status, len(ds), len(e.ch.sent))
	}
	again, created, _ := e.dist.CreateBatch(ctx, distribution.PlanRequest{ClientKey: "b1"})
	if created || again.ID != b1.ID || e.count(`SELECT count(*) FROM discovery_deliveries`) != 2 {
		t.Fatal("idempotent batch creation")
	}
	if b1.RankingVersion == "" || b1.PresentationVersion != presentation.Version || len(b1.Arms) != 2 {
		t.Fatalf("config version recorded: %+v", b1)
	}

	// Send.
	rep2, err := e.dist.SendBatch(ctx, b1.ID)
	if err != nil || rep2.Sent != 2 || rep2.Status != distribution.BatchSent {
		t.Fatalf("send: %+v %v", rep2, err)
	}
	msgsA := e.ch.to(staffA)
	if len(msgsA) == 0 {
		t.Fatal("staff A got nothing")
	}
	// Recorded with purpose + provider message id + delivery linkage.
	dA := deliveryFor(ds, staffA)
	if e.count(`SELECT count(*) FROM messages WHERE direction = 'OUTBOUND' AND metadata->>'entity_type' = 'discovery_delivery'
		AND metadata->>'entity_id' = $1 AND external_message_id LIKE 'wamid.test.%'`, dA.ID.String()) == 0 {
		t.Fatal("outbound discovery messages are recorded with their delivery")
	}
	sentA, _ := e.repo.FindDelivery(ctx, dA.ID)
	if sentA.Status != distribution.DeliverySent || sentA.ProviderMessageID == "" || sentA.MessageID == nil || sentA.Presentation != "session" {
		t.Fatalf("delivery sent: %+v", sentA)
	}
	// Stored media is presented (uploaded once), never re-downloaded.
	foundImage := false
	for _, m := range msgsA {
		if m.Header != nil && m.Header.ProviderRef != "" || m.Kind == channel.KindList {
			foundImage = true
		}
	}
	if !foundImage {
		t.Fatalf("presentation: %+v", msgsA)
	}
	// Idempotent send.
	rep3, _ := e.dist.SendBatch(ctx, b1.ID)
	if !rep3.Already || len(e.ch.to(staffA)) != len(msgsA) {
		t.Fatal("a sent batch is never re-sent")
	}

	// Frequency cap: a second batch inside the cap period is skipped.
	b2, ds2 := e.batch("b2")
	if len(ds2) != 0 {
		// planning already excludes capped buyers
		t.Fatalf("cap at planning: %d deliveries", len(ds2))
	}
	if b2.Plan.ExcludedBuyers[distribution.ReasonFrequencyCap] != 2 {
		t.Fatalf("cap exclusions: %+v", b2.Plan.ExcludedBuyers)
	}
	// Past the cap period, the same listings are deduped (exposure window).
	mustExec(t, e.db, `UPDATE discovery_deliveries SET claimed_at = claimed_at - interval '4 days'`)
	_, ds3 := e.batch("b3")
	if len(ds3) != 0 {
		t.Fatalf("every listing already shown: dedupe, got %d deliveries", len(ds3))
	}
	// Race-proof dedupe at send time: a delivery planned before the exposure
	// is skipped when claimed.
	mustExec(t, e.db, `DELETE FROM discovery_exposures WHERE buyer_id = $1`, b)
	_, ds4 := e.batch("b4", staffB)
	if len(ds4) != 1 {
		t.Fatalf("staff B re-planned: %d", len(ds4))
	}
	mustExec(t, e.db, `INSERT INTO discovery_exposures (delivery_id, buyer_id, listing_id) SELECT $1, $2, unnest(listing_ids) FROM discovery_deliveries WHERE id = $1`, deliveryFor(ds, staffB).ID, b)
	rep5, _ := e.dist.SendBatch(ctx, ds4[0].BatchID)
	if rep5.Sent != 0 || rep5.Skipped[distribution.ReasonDedupe] != 1 {
		t.Fatalf("dedupe at claim time: %+v", rep5)
	}
	_ = a
	_ = bag
}

func TestIntegration_Discovery_OffModeNeverSends(t *testing.T) {
	e := newDiscoveryEnv(t, func(c *distribution.Config) { c.Mode = distribution.ModeOff; c.InternalRecipients = nil })
	ctx := context.Background()
	e.listing("GRA, Benin City", "Samsung TV 55", "electronics", 450000)
	e.buyer(staffA)
	if _, _, err := e.dist.CreateBatch(ctx, distribution.PlanRequest{ClientKey: "x"}); !errors.Is(err, distribution.ErrDiscoveryOff) {
		t.Fatalf("OFF: no batch: %v", err)
	}
	if err := e.dist.InviteConsent(ctx, staffA); !errors.Is(err, distribution.ErrDiscoveryOff) {
		t.Fatalf("OFF: no consent prompt: %v", err)
	}
	if _, err := e.dist.SendBatch(ctx, uuid.New()); !errors.Is(err, distribution.ErrDiscoveryOff) {
		t.Fatalf("OFF: no send: %v", err)
	}
	rep, err := e.dist.Preview(ctx, distribution.PlanRequest{Recipients: []string{staffA}})
	if err != nil || rep.SendAllowed || rep.ExcludedBuyers[distribution.ReasonDiscoveryOff] != 1 {
		t.Fatalf("OFF dry run: %+v %v", rep, err)
	}
	if len(e.ch.sent) != 0 {
		t.Fatal("OFF sends nothing")
	}
}

func TestIntegration_Discovery_StructuredInteractionsNeverMutateOnBadIDs(t *testing.T) {
	e := newDiscoveryEnv(t, nil)
	ctx := context.Background()
	tv := e.listing("GRA, Benin City", "Samsung TV 55", "electronics", 450000)
	a, b := e.buyer(staffA), e.buyer(staffB)
	e.optIn(staffA, "GRA, Benin City")
	e.optIn(staffB, "GRA, Benin City")
	batch, ds := e.batch("i1")
	if _, err := e.dist.SendBatch(ctx, batch.ID); err != nil {
		t.Fatal(err)
	}
	dA, dB := deliveryFor(ds, staffA), deliveryFor(ds, staffB)
	before := e.commerceState()
	bad := map[string]string{
		"dd:not-a-uuid:show": distribution.UnresolvedMalformed,
		"dd:" + uuid.NewString() + ":l:" + tv.PublicReference + ":ask": distribution.UnresolvedUnknown,
		structid.Ask(dB.ID, tv.PublicReference):                        distribution.UnresolvedForeign,
		structid.Ask(dA.ID, "L-ZZZZ99"):                                distribution.UnresolvedListingNotShown,
		"dc:in:v1":                                                     distribution.UnresolvedConsentNoPrompt,
		"opt_0":                                                        distribution.UnresolvedMalformed,
	}
	for raw, reason := range bad {
		msg := e.tap(a, staffA, raw)
		var got string
		_ = e.db.QueryRow(`SELECT reason FROM discovery_interactions WHERE inbound_message_id = $1`, msg).Scan(&got)
		if got != reason {
			t.Errorf("%s: reason %q want %q", raw, got, reason)
		}
	}
	if after := e.commerceState(); after != before {
		t.Fatalf("unresolved interactions changed state: %s -> %s", before, after)
	}
	// An expired delivery: no request, no consent change.
	mustExec(t, e.db, `UPDATE discovery_deliveries SET sent_at = sent_at - interval '8 days' WHERE id = $1`, dA.ID)
	msg := e.tap(a, staffA, structid.Ask(dA.ID, tv.PublicReference))
	var reason string
	_ = e.db.QueryRow(`SELECT reason FROM discovery_interactions WHERE inbound_message_id = $1`, msg).Scan(&reason)
	if reason != distribution.UnresolvedExpired || e.commerceState() != before {
		t.Fatalf("expired delivery: %q", reason)
	}
	if e.count(`SELECT count(*) FROM discovery_interactions WHERE outcome = 'UNRESOLVED'`) != len(bad)+1 {
		t.Fatal("every unresolved interaction is persisted for investigation")
	}
	_ = b
}

// The commerce E2E at the service level: Ask about it -> BuyerRequest
// (origin discovery) -> confirm -> VendorRequest to the listing's vendor
// (listing provenance) -> vendor re-confirms -> Offer with the listing ->
// selection -> Transaction (UNPAID) with the listing; attribution and funnel.
func TestIntegration_Discovery_AskEntersTheExistingCommerceLoop(t *testing.T) {
	e := newDiscoveryEnv(t, nil)
	ctx := context.Background()
	tv := e.listing("GRA, Benin City", "Samsung TV 55", "electronics", 450000)
	e.image(tv, "image")
	e.image(tv, "video")
	// Make it stale (verified 5 days ago) but live: reconfirmation is the path back to fresh.
	mustExec(t, e.db, `UPDATE supply_listings SET last_verified_at = now() - interval '5 days' WHERE id = $1`, tv.ID)
	a := e.buyer(staffA)
	e.optIn(staffA, "GRA, Benin City", "electronics")
	batch, ds := e.batch("c1")
	if _, err := e.dist.SendBatch(ctx, batch.ID); err != nil {
		t.Fatal(err)
	}
	d := deliveryFor(ds, staffA)
	if !d.Candidates[0].NeedsReconfirmation {
		t.Fatal("stale candidate flagged at planning")
	}
	sent := e.ch.to(staffA)
	if !strings.Contains(sent[len(sent)-1].Body, "Availability to be confirmed") {
		t.Fatalf("stale listing never presented as guaranteed: %q", sent[len(sent)-1].Body)
	}
	// Video (when the arm offers it) uses the stored asset.
	e.tap(a, staffA, structid.Video(d.ID, tv.PublicReference))
	// Ask about it — twice (duplicate tap / redelivery): one request.
	e.tap(a, staffA, structid.Ask(d.ID, tv.PublicReference))
	e.tap(a, staffA, structid.Ask(d.ID, tv.PublicReference))
	var reqID uuid.UUID
	if err := e.db.QueryRow(`SELECT id FROM buyer_requests WHERE origin = 'discovery' AND listing_id = $1 AND discovery_delivery_id = $2`, tv.ID, d.ID).Scan(&reqID); err != nil {
		t.Fatalf("discovery-origin request: %v", err)
	}
	if e.count(`SELECT count(*) FROM buyer_requests`) != 1 || e.count(`SELECT count(*) FROM events WHERE event_type = 'LISTING_ENQUIRY'`) != 1 {
		t.Fatal("one request per (delivery, listing)")
	}
	req, _ := NewRequestRepository(e.db).FindByID(ctx, reqID)
	if req.Status != request.StatusNeedsReview || *req.Product != "Samsung TV 55" || req.Origin != request.OriginDiscovery ||
		req.ExtractedData["needs_reconfirmation"] != true {
		t.Fatalf("request copied from the listing, not confirmed: %+v", req)
	}
	// Unconfirmed: routing refuses (existing rule preserved).
	if _, err := e.routing.RouteToVendors(ctx, req, []uuid.UUID{tv.VendorID}, nil, nil); !errors.Is(err, request.ErrNotConfirmed) {
		t.Fatalf("unconfirmed discovery request must not route: %v", err)
	}
	req, err := e.reqSvc.Confirm(ctx, reqID, nil)
	if err != nil {
		t.Fatal(err)
	}
	vrs, err := e.routing.RouteToVendors(ctx, req, []uuid.UUID{tv.VendorID}, nil, nil)
	if err != nil || len(vrs) != 1 || vrs[0].SupplyListingID == nil || *vrs[0].SupplyListingID != tv.ID {
		t.Fatalf("listing-origin vendor request: %+v %v", vrs, err)
	}
	beforeVerified, _ := e.listings.FindByID(ctx, tv.ID)
	v, _ := NewVendorRepository(e.db).FindByID(ctx, tv.VendorID)
	resp := e.routing.ProcessVendorMessage(ctx, v, "Yes still available, 450k")
	if resp == nil || resp.VendorRequestID == nil {
		t.Fatal("vendor response attached")
	}
	afterVerified, _ := e.listings.FindByID(ctx, tv.ID)
	if !afterVerified.LastVerifiedAt.After(*beforeVerified.LastVerifiedAt) || !afterVerified.ExpiresAt.Equal(beforeVerified.ExpiresAt) {
		t.Fatal("vendor re-confirmation refreshes freshness, never the lifecycle ceiling")
	}
	if el := e.svc.Policy().EvaluateListing(afterVerified, nil, time.Now()); el.Freshness.State != supply.Fresh {
		t.Fatal("stale -> reconfirmed -> fresh")
	}
	offers, err := e.offerSvc.BuildForBuyerRequest(ctx, reqID)
	if err != nil || len(offers) != 1 || offers[0].SupplyListingID == nil || *offers[0].SupplyListingID != tv.ID {
		t.Fatalf("offer with listing provenance: %+v %v", offers, err)
	}
	res, err := e.txSvc.Select(ctx, reqID, offers[0].ID, a, transaction.SourceWhatsApp)
	if err != nil || res.Transaction == nil || res.Transaction.SupplyListingID == nil || *res.Transaction.SupplyListingID != tv.ID ||
		res.Transaction.PaymentStatus != "UNPAID" {
		t.Fatalf("transaction with listing provenance, unpaid: %+v %v", res, err)
	}
	// Attribution survives end to end.
	att, err := e.repo.Attribution(ctx, d.ID)
	if err != nil || len(att.Messages) == 0 || len(att.Requests) != 1 || len(att.Offers) != 1 || len(att.Selections) != 1 || len(att.Transactions) != 1 ||
		att.BatchID != batch.ID || att.Arm != d.Arm {
		t.Fatalf("attribution: %+v %v", att, err)
	}
	f, err := e.repo.Funnel(ctx, &batch.ID)
	if err != nil || f.DeliveriesSent != 1 || f.Interacted != 1 || f.Asked != 1 || f.ListingViewed != 1 || f.RequestsCreated != 1 ||
		f.RequestsConfirmed != 1 || f.Offers != 1 || f.Selections != 1 || f.Transactions != 1 {
		t.Fatalf("funnel: %+v %v", f, err)
	}
	// Offer origin is derived (no duplicated origin column).
	if e.count(`SELECT count(*) FROM information_schema.columns WHERE table_name = 'offers' AND column_name = 'origin'`) != 0 {
		t.Fatal("offer origin must be derived, not stored")
	}
}

func TestIntegration_Discovery_PartialFailureAndExpiredSupply(t *testing.T) {
	e := newDiscoveryEnv(t, nil)
	ctx := context.Background()
	tv := e.listing("GRA, Benin City", "Samsung TV 55", "electronics", 450000)
	e.image(tv, "image")
	phone := e.listing("GRA, Benin City", "iPhone 13", "electronics", 520000)
	e.buyer(staffA)
	b := e.buyer(staffB)
	e.buyer(staffC)
	for _, s := range []string{staffA, staffB, staffC} {
		e.optIn(s, "GRA, Benin City", "electronics")
	}
	batch, ds := e.batch("p1")
	if len(ds) != 3 {
		t.Fatalf("3 deliveries, got %d", len(ds))
	}
	e.ch.failTo[staffB] = true // provider rejects B
	e.ch.failUpload = true     // media upload fails: cards degrade to text, never blank
	// Expire the iPhone between planning and sending.
	mustExec(t, e.db, `UPDATE supply_listings SET expires_at = now() - interval '1 second' WHERE id = $1`, phone.ID)
	rep, err := e.dist.SendBatch(ctx, batch.ID)
	if err != nil || rep.Sent != 2 || rep.Failed != 1 || rep.Status != distribution.BatchPartial {
		t.Fatalf("one failure never blanks the batch: %+v %v", rep, err)
	}
	dB, _ := e.repo.FindDelivery(ctx, deliveryFor(ds, staffB).ID)
	if dB.Status != distribution.DeliveryFailed || !strings.Contains(dB.Error, "131047") ||
		e.count(`SELECT count(*) FROM discovery_exposures WHERE buyer_id = $1`, b) != 0 {
		t.Fatalf("failed delivery: no exposure recorded: %+v", dB)
	}
	for _, m := range e.ch.to(staffA) {
		if strings.Contains(m.Summary(), phone.PublicReference) {
			t.Fatal("expired supply is never delivered")
		}
		if m.Header != nil {
			t.Fatal("unpublishable media degrades to a text card")
		}
	}
	// Expired supply stays in the database with its evidence.
	if e.count(`SELECT count(*) FROM supply_listings WHERE id = $1`, phone.ID) != 1 {
		t.Fatal("expired listings are history")
	}
	// Not discoverable (canonical policy).
	res, _ := e.disc.Candidates(ctx, discovery.ViewerContext{Location: "GRA, Benin City"}, discovery.Scope{Limit: 10}, discovery.DefaultParams())
	for _, c := range res.Candidates {
		if c.ListingID == phone.ID {
			t.Fatal("expired listing discovered")
		}
	}
}

func TestIntegration_Discovery_GeographyFromTheCanonicalPolicy(t *testing.T) {
	e := newDiscoveryEnv(t, nil)
	ctx := context.Background()
	tv := e.listing("GRA, Benin City", "Samsung TV 55", "electronics", 450000)
	e.buyer(staffA)
	e.buyer(staffB)
	e.buyer(staffC)
	e.optIn(staffA, "GRA, Benin City")
	e.optIn(staffB, "Ugbowo, Benin City")
	e.optIn(staffC, "Warri")
	planned := func() []string {
		rep, err := e.dist.Preview(ctx, distribution.PlanRequest{})
		if err != nil {
			t.Fatal(err)
		}
		var who []string
		for _, b := range rep.Buyers {
			if b.Planned {
				who = append(who, b.Recipient)
			}
		}
		return who
	}
	if got := planned(); fmt.Sprint(got) != fmt.Sprint([]string{staffA}) {
		t.Fatalf("day 0: local only, got %v", got)
	}
	cfg := e.svc.Policy().Config()
	e.age(tv.ID, cfg.GeoNearbyAfter+time.Hour)
	mustExec(t, e.db, `UPDATE supply_listings SET last_verified_at = now() WHERE id = $1`, tv.ID)
	if got := planned(); fmt.Sprint(got) != fmt.Sprint([]string{staffA, staffB}) {
		t.Fatalf("nearby stage: %v", got)
	}
	e.age(tv.ID, cfg.GeoRegionAfter-cfg.GeoNearbyAfter)
	if got := planned(); fmt.Sprint(got) != fmt.Sprint([]string{staffA, staffB, staffC}) {
		t.Fatalf("region stage: %v", got)
	}
	e.age(tv.ID, cfg.MaxLifecycle)
	if got := planned(); len(got) != 0 {
		t.Fatalf("expired: nobody, got %v", got)
	}
}

// STOP is an opt-out: it is honoured whatever the state/age/ownership of the
// delivery it was tapped on (review finding: an expired STOP tap must not
// leave a buyer subscribed).
func TestIntegration_Discovery_StopTapAlwaysOptsOut(t *testing.T) {
	for name, mk := range map[string]func(e *discoveryEnv, own, other *distribution.Delivery) string{
		"expired delivery": func(e *discoveryEnv, own, other *distribution.Delivery) string {
			mustExec(t, e.db, `UPDATE discovery_deliveries SET sent_at = sent_at - interval '30 days' WHERE id = $1`, own.ID)
			return structid.Stop(own.ID)
		},
		"unknown delivery": func(e *discoveryEnv, own, other *distribution.Delivery) string { return structid.Stop(uuid.New()) },
		"another buyer's delivery": func(e *discoveryEnv, own, other *distribution.Delivery) string {
			return structid.Stop(other.ID)
		},
	} {
		t.Run(name, func(t *testing.T) {
			e := newDiscoveryEnv(t, nil)
			ctx := context.Background()
			e.listing("GRA, Benin City", "Samsung TV 55", "electronics", 450000)
			a, other := e.buyer(staffA), e.buyer(staffB)
			e.optIn(staffA, "GRA, Benin City")
			e.optIn(staffB, "GRA, Benin City")
			b, ds := e.batch("stop-" + name)
			if _, err := e.dist.SendBatch(ctx, b.ID); err != nil {
				t.Fatal(err)
			}
			raw := mk(e, deliveryFor(ds, staffA), deliveryFor(ds, staffB))
			msg := e.tap(a, staffA, raw)
			p, _ := e.repo.Preference(ctx, a)
			if p.Status != distribution.OptedOut {
				t.Fatalf("STOP tapped on a %s left the buyer %s", name, p.Status)
			}
			var outcome string
			_ = e.db.QueryRow(`SELECT outcome FROM discovery_interactions WHERE inbound_message_id = $1`, msg).Scan(&outcome)
			if outcome != distribution.OutcomeApplied {
				t.Fatalf("outcome %s", outcome)
			}
			// The other buyer is untouched.
			if q, _ := e.repo.Preference(ctx, other); q.Status != distribution.OptedIn {
				t.Fatal("another buyer's consent must never change")
			}
		})
	}
}

// A client disconnect (cancelled request context) must not strand a batch.
func TestIntegration_Discovery_SendSurvivesCancelledRequestContext(t *testing.T) {
	e := newDiscoveryEnv(t, nil)
	e.listing("GRA, Benin City", "Samsung TV 55", "electronics", 450000)
	e.buyer(staffA)
	e.optIn(staffA, "GRA, Benin City")
	e.inbound(staffA, "hello")
	b, _ := e.batch("cancel")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rep, err := e.dist.SendBatch(ctx, b.ID)
	if err != nil || rep.Sent != 1 || rep.Status != distribution.BatchSent {
		t.Fatalf("send with a cancelled request context: %+v %v", rep, err)
	}
	if e.count(`SELECT count(*) FROM discovery_deliveries WHERE status = 'SENDING'`) != 0 {
		t.Fatal("no delivery may be left SENDING")
	}
}

// An interrupted send (crash between claim and confirmation) is recovered:
// surfaced as FAILED, cap/exposure released, remaining PLANNED deliveries sent.
func TestIntegration_Discovery_InterruptedSendIsRecovered(t *testing.T) {
	e := newDiscoveryEnv(t, nil)
	ctx := context.Background()
	e.listing("GRA, Benin City", "Samsung TV 55", "electronics", 450000)
	e.buyer(staffA)
	e.buyer(staffB)
	e.optIn(staffA, "GRA, Benin City")
	e.optIn(staffB, "GRA, Benin City")
	e.inbound(staffA, "hello")
	e.inbound(staffB, "hello")
	b, ds := e.batch("interrupted")
	dA := deliveryFor(ds, staffA)
	// Simulate the crash: A was claimed (exposure + cap consumed) and the process died.
	mustExec(t, e.db, `UPDATE discovery_batches SET status = 'SENDING', send_started_at = now() - interval '1 hour', updated_at = now() - interval '1 hour' WHERE id = $1`, b.ID)
	mustExec(t, e.db, `UPDATE discovery_deliveries SET status = 'SENDING', claimed_at = now() - interval '1 hour' WHERE id = $1`, dA.ID)
	mustExec(t, e.db, `INSERT INTO discovery_exposures (delivery_id, buyer_id, listing_id) SELECT $1, buyer_id, l FROM discovery_deliveries d, unnest(d.listing_ids) l WHERE d.id = $1`, dA.ID)
	rep, err := e.dist.SendBatch(ctx, b.ID)
	if err != nil || rep.Already {
		t.Fatalf("a stale SENDING batch must be resumable: %+v %v", rep, err)
	}
	var status, errText string
	_ = e.db.QueryRow(`SELECT status, coalesce(error,'') FROM discovery_deliveries WHERE id = $1`, dA.ID).Scan(&status, &errText)
	if status != distribution.DeliveryFailed || errText == "" {
		t.Fatalf("interrupted delivery: %s %q", status, errText)
	}
	if e.count(`SELECT count(*) FROM discovery_exposures WHERE delivery_id = $1`, dA.ID) != 0 {
		t.Fatal("exposure released")
	}
	if rep.Sent != 1 || e.count(`SELECT count(*) FROM discovery_deliveries WHERE status = 'SENDING'`) != 0 {
		t.Fatalf("remaining delivery sent, nothing stuck: %+v", rep)
	}
}
