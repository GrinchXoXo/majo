package postgres

// Phase 4A.2 Postgres integration tests: vendor media + captions + text ->
// grouped SupplySignal. Real PostgreSQL, the real signed-webhook handler,
// the real routing service (vendor-message classification, VendorResponse
// path), real media ingestion against the fake Graph endpoint, the real
// durable worker. The extraction service is replaced by a small
// deterministic stand-in with the same product/price/category behaviour for
// the test sentences (the Python extractor itself is exercised by the
// process-level E2E script).

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/httpserver"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/media"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/supply"
)

// testExtractor: deterministic stand-in for /extract-supply.
type testExtractor struct{}

var (
	kPrice     = regexp.MustCompile(`(?i)(\d+(?:\.\d+)?)\s*k\b`)
	commaPrice = regexp.MustCompile(`(\d{1,3}(?:,\d{3})+)`)
)

func (testExtractor) Extract(ctx context.Context, req ai.ExtractionRequest) (*ai.ExtractionResult, error) {
	return &ai.ExtractionResult{}, nil
}
func (testExtractor) ExtractSupply(ctx context.Context, text string) (*ai.SupplyExtractionResult, error) {
	r := &ai.SupplyExtractionResult{AvailabilityStatus: "AVAILABLE"}
	l := strings.ToLower(text)
	for _, p := range []struct{ kw, product, category string }{
		{"bag", "bag", "fashion"}, {"shoe", "shoe", "fashion"}, {"iphone", "iphone 13", "electronics"}, {"ps5", "ps5", "electronics"},
	} {
		if strings.Contains(l, p.kw) {
			prod, cat := p.product, p.category
			r.Product, r.Category = &prod, &cat
			break
		}
	}
	if m := kPrice.FindStringSubmatch(text); m != nil {
		f, _ := strconv.ParseFloat(m[1], 64)
		v := int64(f * 1000)
		r.Price = &v
	} else if m := commaPrice.FindStringSubmatch(text); m != nil {
		v, _ := strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
		r.Price = &v
	}
	if r.Product != nil {
		r.Confidence += 0.5
	}
	if r.Price != nil {
		r.Confidence += 0.3
	}
	return r, nil
}
func (testExtractor) ExtractVendorResponse(ctx context.Context, text string) (*ai.VendorResponseExtractionResult, error) {
	return &ai.VendorResponseExtractionResult{Status: "AVAILABLE", ReviewState: "REVIEWED", ExtractionMethod: "test", Confidence: 0.7}, nil
}
func (testExtractor) Healthy(ctx context.Context) error { return nil }

type noopSender struct{}

func (noopSender) SendText(ctx context.Context, to, body string) error { return nil }

type supplyEnv struct {
	*mediaEnv
	intake  *supply.Intake
	routing *routing.Service
	repo    *SupplyGroupingRepository
	window  time.Duration
}

func newSupplyEnv(t *testing.T) *supplyEnv {
	t.Helper()
	m := newMediaEnv(t)
	s := &supplyEnv{mediaEnv: m, repo: NewSupplyGroupingRepository(m.db), window: 120 * time.Second}
	vendors := NewVendorRepository(m.db)
	s.routing = routing.NewService(vendors, NewSupplyRepository(m.db), NewRoutingRepository(m.db), NewRequestRepository(m.db),
		testExtractor{}, NewEventWriter(m.db), NewAuditWriter(m.db), noopSender{}, discard())
	s.intake = s.newIntake()
	s.routing.SetSupplyIntake(s.intake)
	conv := NewConversationRepository(m.db)
	m.webhook = httpserver.NewWhatsAppWebhookHandler(m.provider(), conv, conv, NewBuyerRepository(m.db), vendors,
		NewEventWriter(m.db), nil, s.routing, jobs.NewRunner(discard(), 1, 64), "verify", false, discard())
	ing := m.ingestor()
	ing.SetSettleListener(s.intake)
	m.webhook.SetMediaIngestion(ing)
	return s
}

func (s *supplyEnv) newIntake() *supply.Intake {
	return supply.NewIntake(s.repo, testExtractor{}, s.jobs, NewVendorRepository(s.db), s.window, 5, discard())
}

// worker runs both durable job types, as the server does.
func (s *supplyEnv) fullWorker(id string, types ...string) *jobs.Worker {
	ing := s.ingestor()
	ing.SetSettleListener(s.intake)
	w := jobs.NewWorker(s.jobs, jobs.WorkerConfig{ID: id, PollInterval: 5 * time.Millisecond, Lease: time.Minute,
		Retry: jobs.RetryPolicy{BaseDelay: 5 * time.Millisecond, MaxDelay: 20 * time.Millisecond}}, discard())
	all := len(types) == 0
	for _, t := range types {
		all = all || t == "*"
	}
	if all || contains(types, media.JobTypeDownload) {
		w.Register(media.JobTypeDownload, ing.Handle, ing.OnFailure)
	}
	if all || contains(types, supply.JobTypeGroup) {
		w.Register(supply.JobTypeGroup, s.intake.Handle, nil)
	}
	return w
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func payloadAt(from, wamid string, at time.Time, kind, body string) string {
	var msg map[string]any
	ts := strconv.FormatInt(at.Unix(), 10)
	switch kind {
	case "text":
		msg = map[string]any{"from": from, "id": wamid, "timestamp": ts, "type": "text", "text": map[string]string{"body": body}}
	default: // "image:<mediaID>" or "video:<mediaID>", body = caption
		parts := strings.SplitN(kind, ":", 2)
		m := map[string]any{"id": parts[1], "mime_type": "image/jpeg"}
		if body != "" {
			m["caption"] = body
		}
		msg = map[string]any{"from": from, "id": wamid, "timestamp": ts, "type": parts[0], parts[0]: m}
	}
	b, _ := json.Marshal(map[string]any{"entry": []any{map[string]any{"changes": []any{map[string]any{"value": map[string]any{
		"contacts": []any{map[string]any{"wa_id": from, "profile": map[string]string{"name": "Vendor"}}},
		"messages": []any{msg}}}}}}})
	return string(b)
}

// send posts a message and, for texts, waits until routing has processed it
// (vendor text routing runs on the in-memory runner, as in production).
func (s *supplyEnv) send(from, wamid string, at time.Time, kind, body string) {
	s.t.Helper()
	s.post(payloadAt(from, wamid, at, kind, body))
	if kind == "text" {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			// Routing finished: the text became a VendorResponse, a signal's
			// source (fast path), or a deferred grouping job.
			if s.count(`SELECT count(*) FROM messages m WHERE m.external_message_id = $1 AND (
				EXISTS (SELECT 1 FROM supply_signal_sources ss WHERE ss.message_id = m.id) OR
				EXISTS (SELECT 1 FROM jobs j WHERE j.job_type = 'SUPPLY_GROUPING' AND j.idempotency_key = m.id::text || ':text') OR
				EXISTS (SELECT 1 FROM vendor_responses r WHERE r.raw_message = $2))`, wamid, body) > 0 {
				return
			}
			time.Sleep(5 * time.Millisecond)
		}
		s.t.Fatalf("text %s was not processed by routing", wamid)
	}
}

type signalRow struct {
	ID         uuid.UUID
	Product    *string
	Category   *string
	Price      *int64
	Currency   string
	Raw        string
	Confidence float64
	Grouping   string
	Evidence   *supply.Evidence
}

func (s *supplyEnv) signals(vendorID uuid.UUID) []signalRow {
	s.t.Helper()
	rows, err := s.db.Query(`SELECT id, product, category, price, currency, raw_message, confidence, COALESCE(extracted_data->>'grouping','')
		FROM supply_signals WHERE vendor_id = $1 ORDER BY created_at, id`, vendorID)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	var out []signalRow
	for rows.Next() {
		var r signalRow
		var id string
		if err := rows.Scan(&id, &r.Product, &r.Category, &r.Price, &r.Currency, &r.Raw, &r.Confidence, &r.Grouping); err != nil {
			s.t.Fatal(err)
		}
		r.ID = uuid.MustParse(id)
		out = append(out, r)
	}
	for i := range out {
		ev, err := s.repo.Evidence(context.Background(), out[i].ID)
		if err != nil {
			s.t.Fatal(err)
		}
		out[i].Evidence = ev
	}
	return out
}

func (s *supplyEnv) assetOf(wamid string) uuid.UUID {
	s.t.Helper()
	a := s.linkedAsset(wamid)
	if a == nil {
		s.t.Fatalf("%s has no asset", wamid)
	}
	return a.ID
}

func sp(x *string) string {
	if x == nil {
		return "<nil>"
	}
	return *x
}

func ago(d time.Duration) time.Time { return time.Now().Add(-d).Truncate(time.Second) }

// --- §70 acceptance: three photos + caption -> ONE SupplySignal ------------------

func TestIntegration_Grouping_ThreePhotosAndCaptionBecomeOneSignal(t *testing.T) {
	s := newSupplyEnv(t)
	wa := "2348041110001"
	vendor := s.vendor("Ada Bags", wa)
	for i, b := range [][]byte{photo1, photo2, photo3} {
		s.graph.AddMedia(fmt.Sprint("g-", i+1), b, "image/jpeg")
	}
	t0 := ago(10 * time.Minute) // the grouping window has long elapsed
	s.send(wa, "wamid.G1", t0, "image:g-1", "")
	s.send(wa, "wamid.G2", t0.Add(10*time.Second), "image:g-2", "")
	s.send(wa, "wamid.G3", t0.Add(18*time.Second), "image:g-3", "")
	s.send(wa, "wamid.G4", t0.Add(25*time.Second), "text", "Imported ladies bags ₦18,500")
	s.send(wa, "wamid.G2", t0.Add(10*time.Second), "image:g-2", "") // Meta redelivery
	s.drain(s.fullWorker("w"))

	sigs := s.signals(vendor)
	if len(sigs) != 1 {
		t.Fatalf("expected exactly one SupplySignal, got %d", len(sigs))
	}
	g := sigs[0]
	if sp(g.Product) != "bag" || sp(g.Category) != "fashion" || g.Price == nil || *g.Price != 18500 || g.Currency != "NGN" ||
		g.Raw != "Imported ladies bags ₦18,500" || g.Confidence != 0.8 || g.Grouping != "grouped:first_message" {
		t.Fatalf("signal fields wrong: %+v product=%s price=%v", g, sp(g.Product), g.Price)
	}
	wantMsgs := []uuid.UUID{s.messageID("wamid.G1"), s.messageID("wamid.G2"), s.messageID("wamid.G3"), s.messageID("wamid.G4")}
	wantAssets := []uuid.UUID{s.assetOf("wamid.G1"), s.assetOf("wamid.G2"), s.assetOf("wamid.G3")}
	if fmt.Sprint(g.Evidence.SourceMessageIDs) != fmt.Sprint(wantMsgs) || fmt.Sprint(g.Evidence.MediaAssetIDs) != fmt.Sprint(wantAssets) {
		t.Fatalf("provenance wrong:\n got %v / %v\nwant %v / %v", g.Evidence.SourceMessageIDs, g.Evidence.MediaAssetIDs, wantMsgs, wantAssets)
	}
	// SignalSource -> message -> vendor, and asset -> same vendor, without guessing.
	if s.count(`SELECT count(*) FROM supply_signal_sources ss JOIN messages m ON m.id = ss.message_id
		JOIN conversations c ON c.id = m.conversation_id JOIN vendors v ON v.whatsapp_external_id = c.external_id
		LEFT JOIN media_assets a ON a.id = ss.media_asset_id
		WHERE ss.supply_signal_id = $1 AND v.id = $2 AND (a.id IS NULL OR a.owner_vendor_id = v.id)`, g.ID, vendor) != 4 {
		t.Fatal("evidence chain signal -> messages -> vendor -> assets broken")
	}
	if n := s.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_SIGNAL_CREATED' AND entity_id = $1
		AND jsonb_array_length(payload->'source_message_ids') = 4 AND jsonb_array_length(payload->'media_asset_ids') = 3
		AND payload->>'supply_signal_id' = $1::text AND payload->>'vendor_id' = $2::text`, g.ID, vendor); n != 1 {
		t.Fatalf("expected one SUPPLY_SIGNAL_CREATED with provenance, got %d", n)
	}
	if s.count(`SELECT count(*) FROM supply_listings`) != 0 {
		t.Fatal("4A.2 must not create SupplyListings")
	}

	// Reprocessing every grouping job (restart / retry / redeploy) changes nothing.
	rows, _ := s.db.Query(`SELECT id, payload FROM jobs WHERE job_type = 'SUPPLY_GROUPING'`)
	var again []*jobs.Job
	for rows.Next() {
		var id string
		var p []byte
		_ = rows.Scan(&id, &p)
		j := &jobs.Job{ID: uuid.MustParse(id), Type: supply.JobTypeGroup}
		_ = json.Unmarshal(p, &j.Payload)
		again = append(again, j)
	}
	rows.Close()
	if len(again) == 0 {
		t.Fatal("expected grouping jobs")
	}
	for _, j := range again {
		if err := s.newIntake().Handle(context.Background(), j); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.signals(vendor)) != 1 || s.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_SIGNAL_CREATED'`) != 1 {
		t.Fatal("reprocessing created a duplicate signal or event")
	}
}

// --- §71 / Test 1: text-only supply still works, now with provenance -----------

func TestIntegration_Grouping_TextOnlySupplyIsImmediate(t *testing.T) {
	s := newSupplyEnv(t)
	wa := "2348041110002"
	vendor := s.vendor("Mike Gadgets", wa)
	s.send(wa, "wamid.T1", ago(time.Second), "text", "iPhone 13 128GB, 620k")
	sigs := s.signals(vendor) // no worker has run: the text path is still immediate
	if len(sigs) != 1 || sp(sigs[0].Product) != "iphone 13" || *sigs[0].Price != 620000 || sigs[0].Grouping != "text_only" {
		t.Fatalf("expected one immediate text signal, got %+v", sigs)
	}
	if fmt.Sprint(sigs[0].Evidence.SourceMessageIDs) != fmt.Sprint([]uuid.UUID{s.messageID("wamid.T1")}) || len(sigs[0].Evidence.MediaAssetIDs) != 0 {
		t.Fatal("text signal must record its source message and no media")
	}
	if s.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_SIGNAL_CREATED' AND entity_id = $1`, sigs[0].ID) != 1 {
		t.Fatal("expected SUPPLY_SIGNAL_CREATED")
	}
}

// --- Tests 2, 4: caption on the image; captions spread over images --------------

func TestIntegration_Grouping_CaptionedImagesAndMultipleCaptions(t *testing.T) {
	s := newSupplyEnv(t)
	waA, waB := "2348041110003", "2348041110004"
	vA, vB := s.vendor("A", waA), s.vendor("B", waB)
	s.graph.AddMedia("c-1", photo1, "image/jpeg")
	s.graph.AddMedia("c-2", photo2, "image/jpeg")
	s.graph.AddMedia("c-3", photo3, "image/jpeg")
	t0 := ago(10 * time.Minute)
	s.send(waA, "wamid.CA", t0, "image:c-1", "Imported bags ₦18,500")
	s.send(waB, "wamid.CB1", t0, "image:c-2", "iPhone 13")
	s.send(waB, "wamid.CB2", t0.Add(20*time.Second), "image:c-3", "128GB 620k")
	s.drain(s.fullWorker("w"))

	a := s.signals(vA)
	if len(a) != 1 || sp(a[0].Product) != "bag" || *a[0].Price != 18500 || len(a[0].Evidence.MediaAssetIDs) != 1 ||
		a[0].Evidence.MediaAssetIDs[0] != s.assetOf("wamid.CA") {
		t.Fatalf("image + caption: expected one signal with one asset, got %+v", a)
	}
	b := s.signals(vB)
	if len(b) != 1 || b[0].Raw != "iPhone 13\n128GB 620k" || sp(b[0].Product) != "iphone 13" || *b[0].Price != 620000 ||
		len(b[0].Evidence.MediaAssetIDs) != 2 || len(b[0].Evidence.SourceMessageIDs) != 2 {
		t.Fatalf("two captions: expected one signal combining both, got %+v", b)
	}
	// The original messages are untouched evidence.
	if s.count(`SELECT count(*) FROM messages WHERE external_message_id = 'wamid.CB1' AND content->'media'->>'caption' = 'iPhone 13'`) != 1 {
		t.Fatal("message content must not be rewritten")
	}
}

// --- Test 5: window boundary uses the configured parameter --------------------

func TestIntegration_Grouping_WindowBoundary(t *testing.T) {
	s := newSupplyEnv(t)
	in, out := "2348041110005", "2348041110006"
	vIn, vOut := s.vendor("In", in), s.vendor("Out", out)
	for _, id := range []string{"b-1", "b-2", "b-3"} {
		s.graph.AddMedia(id, photo1, "image/jpeg")
	}
	t0 := ago(20 * time.Minute)
	s.send(in, "wamid.BI1", t0, "image:b-1", "")
	s.send(in, "wamid.BI2", t0.Add(60*time.Second), "image:b-2", "")
	s.send(in, "wamid.BI3", t0.Add(119*time.Second), "text", "bags 18,500")
	s.send(out, "wamid.BO1", t0, "image:b-3", "")
	s.send(out, "wamid.BO2", t0.Add(121*time.Second), "text", "bags 18,500")
	s.drain(s.fullWorker("w"))

	if sig := s.signals(vIn); len(sig) != 1 || len(sig[0].Evidence.SourceMessageIDs) != 3 || len(sig[0].Evidence.MediaAssetIDs) != 1 {
		t.Fatalf("119s: expected one grouped signal (2 photos share bytes -> 1 asset), got %+v", sig)
	}
	sig := s.signals(vOut)
	if len(sig) != 1 || len(sig[0].Evidence.SourceMessageIDs) != 1 || len(sig[0].Evidence.MediaAssetIDs) != 0 || sig[0].Grouping != "text_only" {
		t.Fatalf("121s: the text must not absorb the photo, got %+v", sig)
	}
	if s.linkedAsset("wamid.BO1") == nil {
		t.Fatal("the ungrouped photo is still stored")
	}
}

// --- Test 6: distinct products ------------------------------------------------

func TestIntegration_Grouping_DistinctProductsAreDistinctSignals(t *testing.T) {
	s := newSupplyEnv(t)
	wa := "2348041110007"
	v := s.vendor("V", wa)
	t0 := ago(5 * time.Minute)
	s.send(wa, "wamid.D1", t0, "text", "iPhone 13 620k")
	s.send(wa, "wamid.D2", t0.Add(60*time.Second), "text", "PS5 650k")
	s.drain(s.fullWorker("w"))
	sig := s.signals(v)
	if len(sig) != 2 || sp(sig[0].Product) != "iphone 13" || sp(sig[1].Product) != "ps5" {
		t.Fatalf("expected two signals, got %+v", sig)
	}
}

// seedActiveRequest gives the vendor one SENT VendorRequest for `product`.
func (s *supplyEnv) seedActiveRequest(vendorID uuid.UUID, product, ref string) {
	b, c, r := uuid.New(), uuid.New(), uuid.New()
	mustExec(s.t, s.db, `INSERT INTO buyers (id, external_user_id) VALUES ($1, $2)`, b, "buyer-"+ref)
	mustExec(s.t, s.db, `INSERT INTO conversations (id, external_id, buyer_id) VALUES ($1, $2, $3)`, c, "buyer-"+ref, b)
	mustExec(s.t, s.db, `INSERT INTO buyer_requests (id, conversation_id, buyer_id, status, product, raw_message, confirmed_at)
		VALUES ($1, $2, $3, 'CONFIRMED', $4, 'need it', now())`, r, c, b, product)
	mustExec(s.t, s.db, `INSERT INTO vendor_requests (buyer_request_id, vendor_id, public_reference, status, sent_at)
		VALUES ($1, $2, $3, 'SENT', now())`, r, vendorID, ref)
}

// --- Tests 7, 8, 9 / §72-74: VendorRequest safety --------------------------------

func TestIntegration_Grouping_VendorRequestSafety(t *testing.T) {
	s := newSupplyEnv(t)
	wa := "2348041110008"
	v := s.vendor("V", wa)
	s.seedActiveRequest(v, "iphone 13", "MM-410001")
	s.graph.AddMedia("vr-yes", photo1, "image/jpeg")
	s.graph.AddMedia("vr-bags", photo2, "image/jpeg")
	t0 := ago(10 * time.Minute)

	// While the request is active:
	// §74: photo + "Yes" -> ambiguous -> existing behaviour (stored, not processed).
	s.send(wa, "wamid.V2", t0, "image:vr-yes", "Yes")
	// §73: photo + clearly unrelated supply -> SupplySignal.
	s.send(wa, "wamid.V3", t0.Add(3*time.Minute), "image:vr-bags", "Imported ladies bags ₦18,500")
	// §72: a text reply is a VendorResponse, not supply.
	s.send(wa, "wamid.V1", t0.Add(6*time.Minute), "text", "Yes boss, I get am for 620k")
	s.drain(s.fullWorker("w"))

	if s.count(`SELECT count(*) FROM vendor_responses r JOIN vendor_requests vr ON vr.id = r.vendor_request_id
		WHERE vr.public_reference = 'MM-410001' AND r.raw_message = 'Yes boss, I get am for 620k'`) != 1 {
		t.Fatal("the reply must be a VendorResponse attached to the active request")
	}
	if s.count(`SELECT count(*) FROM supply_candidate_messages c JOIN messages m ON m.id = c.message_id
		WHERE m.external_message_id IN ('wamid.V1', 'wamid.V2')`) != 0 {
		t.Fatal("the reply and the ambiguous photo must not become supply candidates")
	}
	if s.linkedAsset("wamid.V2") == nil {
		t.Fatal("ambiguous media is still stored (4A.1)")
	}
	sig := s.signals(v)
	if len(sig) != 1 || sp(sig[0].Product) != "bag" || fmt.Sprint(sig[0].Evidence.SourceMessageIDs) != fmt.Sprint([]uuid.UUID{s.messageID("wamid.V3")}) {
		t.Fatalf("expected exactly one signal from the unrelated bags photo, got %+v", sig)
	}
	if s.count(`SELECT count(*) FROM supply_candidate_messages c JOIN messages m ON m.id = c.message_id
		WHERE m.external_message_id = 'wamid.V3' AND c.reason = $1`, routing.MediaSupplyUnrelatedCaption) != 1 {
		t.Fatal("the classification reason must be recorded")
	}
}

// --- Test 10: media only -> no invented fields, no signal -----------------------

func TestIntegration_Grouping_MediaOnlyCreatesNoSignal(t *testing.T) {
	s := newSupplyEnv(t)
	wa := "2348041110009"
	v := s.vendor("V", wa)
	s.graph.AddMedia("mo-1", photo1, "image/jpeg")
	s.graph.AddMedia("mo-2", photo2, "image/jpeg")
	t0 := ago(10 * time.Minute)
	s.send(wa, "wamid.MO1", t0, "image:mo-1", "")
	s.send(wa, "wamid.MO2", t0.Add(5*time.Second), "image:mo-2", "")
	s.drain(s.fullWorker("w"))
	if len(s.signals(v)) != 0 {
		t.Fatal("photos without any text must not produce a signal (no invented product/price)")
	}
	if s.linkedAsset("wamid.MO1") == nil || s.linkedAsset("wamid.MO2") == nil {
		t.Fatal("the media is preserved")
	}
	if s.count(`SELECT count(*) FROM supply_candidate_messages WHERE vendor_id = $1`, v) != 2 {
		t.Fatal("the candidates (evidence index) are preserved")
	}
}

// --- Test 12: same product later is new evidence --------------------------------

func TestIntegration_Grouping_SameProductLaterIsANewSignal(t *testing.T) {
	s := newSupplyEnv(t)
	wa := "2348041110010"
	v := s.vendor("V", wa)
	s.send(wa, "wamid.S1", ago(26*time.Hour), "text", "bags 18,500")
	s.send(wa, "wamid.S2", ago(time.Minute), "text", "bags 18,500")
	if sig := s.signals(v); len(sig) != 2 || sig[0].ID == sig[1].ID {
		t.Fatalf("a resubmission is new evidence, got %d signals", len(sig))
	}
}

// --- Tests 14, 15: media not stored yet; partial failure ----------------------

func TestIntegration_Grouping_WaitsForMediaAndHandlesPartialFailure(t *testing.T) {
	s := newSupplyEnv(t)
	wa := "2348041110011"
	v := s.vendor("V", wa)
	s.graph.AddMedia("pf-1", photo1, "image/jpeg")
	s.graph.AddMedia("pf-2", photo2, "image/jpeg")
	s.graph.AddMedia("pf-3", photo3, "image/jpeg")
	s.graph.FailMetadata("pf-3", 404) // expired: this photo can never be stored
	t0 := ago(10 * time.Minute)
	s.send(wa, "wamid.P1", t0, "image:pf-1", "")
	s.send(wa, "wamid.P2", t0.Add(5*time.Second), "image:pf-2", "")
	s.send(wa, "wamid.P3", t0.Add(10*time.Second), "image:pf-3", "")
	s.send(wa, "wamid.P4", t0.Add(20*time.Second), "text", "Imported ladies bags ₦18,500")

	// Grouping alone, before any download finished: must wait, not invent media.
	gOnly := s.fullWorker("g", supply.JobTypeGroup)
	for i := 0; i < 20; i++ {
		_, _ = gOnly.RunOnce(context.Background(), "g")
	}
	if len(s.signals(v)) != 0 {
		t.Fatal("a signal must not be created while its media is still downloading")
	}

	s.drain(s.fullWorker("w"))
	sig := s.signals(v)
	if len(sig) != 1 {
		t.Fatalf("expected one signal once downloads finished, got %d", len(sig))
	}
	if len(sig[0].Evidence.SourceMessageIDs) != 4 || len(sig[0].Evidence.MediaAssetIDs) != 2 {
		t.Fatalf("expected 4 sources and only the 2 stored assets, got %+v", sig[0].Evidence)
	}
	if s.count(`SELECT count(*) FROM supply_signal_sources WHERE message_id = $1 AND media_asset_id IS NULL`, s.messageID("wamid.P3")) != 1 {
		t.Fatal("the failed photo is recorded as evidence without an asset")
	}
	if s.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_SIGNAL_CREATED' AND entity_id = $1
		AND payload->'media_unavailable_message_ids' ? $2`, sig[0].ID, s.messageID("wamid.P3").String()) != 1 {
		t.Fatal("the event must state which media is unavailable")
	}
	for _, id := range sig[0].Evidence.MediaAssetIDs {
		if s.count(`SELECT count(*) FROM media_assets WHERE id = $1`, id) != 1 {
			t.Fatal("a signal may only reference stored assets")
		}
	}
}

// --- §45/46: caption first, photos after -> added to that signal ----------------

func TestIntegration_Grouping_LatePhotosJoinThePrecedingTextSignal(t *testing.T) {
	s := newSupplyEnv(t)
	wa := "2348041110012"
	v := s.vendor("V", wa)
	s.graph.AddMedia("la-1", photo1, "image/jpeg")
	s.graph.AddMedia("la-2", photo2, "image/jpeg")
	t0 := ago(10 * time.Minute)
	s.send(wa, "wamid.L1", t0, "text", "Imported ladies bags ₦18,500") // immediate signal
	s.send(wa, "wamid.L2", t0.Add(30*time.Second), "image:la-1", "")
	s.send(wa, "wamid.L3", t0.Add(40*time.Second), "image:la-2", "")
	s.drain(s.fullWorker("w"))
	sig := s.signals(v)
	if len(sig) != 1 || len(sig[0].Evidence.SourceMessageIDs) != 3 || len(sig[0].Evidence.MediaAssetIDs) != 2 {
		t.Fatalf("expected the photos to join the text signal, got %+v", sig)
	}
	if s.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_SIGNAL_UPDATED' AND entity_id = $1
		AND payload->>'change' = 'media_added_late'`, sig[0].ID) != 1 {
		t.Fatal("expected one SUPPLY_SIGNAL_UPDATED")
	}
	if s.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_SIGNAL_CREATED'`) != 1 {
		t.Fatal("no second signal")
	}
}

// --- §43/44: ordering by the vendor's send time, not arrival ---------------------

func TestIntegration_Grouping_OrdersByProviderTimestamp(t *testing.T) {
	s := newSupplyEnv(t)
	wa := "2348041110013"
	v := s.vendor("V", wa)
	s.graph.AddMedia("o-1", photo1, "image/jpeg")
	s.graph.AddMedia("o-2", photo2, "image/jpeg")
	t0 := ago(10 * time.Minute)
	s.send(wa, "wamid.O2", t0.Add(10*time.Second), "image:o-2", "") // arrives first, sent second
	s.send(wa, "wamid.O1", t0, "image:o-1", "")
	s.send(wa, "wamid.O3", t0.Add(20*time.Second), "text", "bags 18,500")
	s.drain(s.fullWorker("w"))
	sig := s.signals(v)
	want := []uuid.UUID{s.messageID("wamid.O1"), s.messageID("wamid.O2"), s.messageID("wamid.O3")}
	if len(sig) != 1 || fmt.Sprint(sig[0].Evidence.SourceMessageIDs) != fmt.Sprint(want) {
		t.Fatalf("evidence must follow send order, got %+v", sig)
	}
}

// --- Test 16 / §41: concurrent processing ---------------------------------------

func TestIntegration_Grouping_ConcurrentEvaluationYieldsOneSignal(t *testing.T) {
	s := newSupplyEnv(t)
	wa := "2348041110014"
	v := s.vendor("V", wa)
	s.graph.AddMedia("cc-1", photo1, "image/jpeg")
	s.graph.AddMedia("cc-2", photo2, "image/jpeg")
	t0 := ago(10 * time.Minute)
	s.send(wa, "wamid.CC1", t0, "image:cc-1", "")
	s.send(wa, "wamid.CC2", t0.Add(5*time.Second), "image:cc-2", "")
	s.send(wa, "wamid.CC3", t0.Add(9*time.Second), "text", "bags 18,500")
	mw := s.fullWorker("media-only", media.JobTypeDownload)
	for s.count(`SELECT count(*) FROM jobs WHERE job_type = 'MEDIA_DOWNLOAD' AND status <> 'SUCCEEDED'`) > 0 {
		_, _ = mw.RunOnce(context.Background(), "m")
	}
	if len(s.signals(v)) != 0 {
		t.Fatal("no grouping job has run yet")
	}
	ids := []uuid.UUID{s.messageID("wamid.CC1"), s.messageID("wamid.CC2"), s.messageID("wamid.CC3")}

	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			j := &jobs.Job{ID: uuid.New(), Type: supply.JobTypeGroup, Payload: map[string]any{"message_id": ids[i%3].String()}}
			if err := s.newIntake().Handle(context.Background(), j); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if len(s.signals(v)) != 1 || s.count(`SELECT count(*) FROM events WHERE event_type = 'SUPPLY_SIGNAL_CREATED'`) != 1 {
		t.Fatalf("12 concurrent evaluations must yield one signal and one event, got %d", len(s.signals(v)))
	}
}

// --- schema --------------------------------------------------------------------

func TestIntegration_Migration0009Constraints(t *testing.T) {
	s := newSupplyEnv(t)
	wa := "2348041110015"
	v := s.vendor("V", wa)
	s.send(wa, "wamid.K1", ago(time.Second), "text", "bags 18,500")
	sig := s.signals(v)[0]
	other := uuid.New()
	mustExec(t, s.db, `INSERT INTO supply_signals (id, vendor_id, raw_message) VALUES ($1, $2, 'x')`, other, v)
	if _, err := s.db.Exec(`INSERT INTO supply_signal_sources (supply_signal_id, position, message_id) VALUES ($1, 0, $2)`,
		other, s.messageID("wamid.K1")); err == nil || !strings.Contains(err.Error(), "supply_signal_sources_message_id_key") {
		t.Fatalf("a message must evidence at most one signal, got %v", err)
	}
	if _, err := s.db.Exec(`INSERT INTO supply_signal_sources (supply_signal_id, position, message_id) VALUES ($1, 0, $2)`,
		sig.ID, uuid.New()); err == nil {
		t.Fatal("unknown message must be rejected")
	}
	if _, err := s.db.Exec(`INSERT INTO supply_candidate_messages (message_id, vendor_id, conversation_id, kind, message_at, reason)
		SELECT id, $1, conversation_id, 'audio', now(), 'x' FROM messages WHERE external_message_id = 'wamid.K1'`, v); err == nil {
		t.Fatal("candidate kind is constrained")
	}
	var meta string
	_ = s.db.QueryRow(`SELECT metadata->>'provider_timestamp' FROM messages WHERE external_message_id = 'wamid.K1'`).Scan(&meta)
	if _, err := time.Parse(time.RFC3339, meta); err != nil {
		t.Fatalf("provider timestamp must be recorded at creation, got %q", meta)
	}
	_ = conversation.ErrDuplicateMessage
}
