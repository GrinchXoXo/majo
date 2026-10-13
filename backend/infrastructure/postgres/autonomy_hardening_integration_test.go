package postgres

// Phase 4C hardening: Postgres integration tests for vendor-request
// deadlines and request recovery, queued buyer notices, provider-reported
// failures and operator holds. The engine runs against real repositories;
// only the router's matching (candidates) and the WhatsApp sender are fakes.
// Assertions are on persisted state and on what was sent.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/channel"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/matching"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/vendor"
	"github.com/middleman/backend/internal/workflow"
)

// ---- fakes -----------------------------------------------------------------------------

type sentMsg struct{ to, body, kind string }

type fakeMessenger struct {
	mu   sync.Mutex
	sent []sentMsg
	fail func(to, body, kind string) error // nil = accept
}

func (m *fakeMessenger) SendText(ctx context.Context, to, body string) error {
	kind := ""
	if p, ok := conversation.OutboundPurposeFrom(ctx); ok {
		kind = p.Kind
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		if err := m.fail(to, body, kind); err != nil {
			return err
		}
	}
	m.sent = append(m.sent, sentMsg{to, body, kind})
	return nil
}

func (m *fakeMessenger) byKind(kind string) []sentMsg {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []sentMsg
	for _, s := range m.sent {
		if s.kind == kind {
			out = append(out, s)
		}
	}
	return out
}

type fakeRouter struct {
	db         *sql.DB
	cands      func() ([]matching.Candidate, error)
	routeErr   error
	routeCall  int32
	resend     func() error
	resendCall int32
}

func (r *fakeRouter) SuggestVendors(ctx context.Context, id uuid.UUID) ([]matching.Candidate, *request.BuyerRequest, error) {
	if r.cands == nil {
		return nil, nil, nil
	}
	c, err := r.cands()
	return c, nil, err
}

func (r *fakeRouter) RouteAs(ctx context.Context, req *request.BuyerRequest, vendorIDs []uuid.UUID, _ map[uuid.UUID]int,
	_ map[uuid.UUID]map[string]any, actor event.ActorType, reason string) ([]*routing.VendorRequest, error) {
	atomic.AddInt32(&r.routeCall, 1)
	if r.routeErr != nil {
		return nil, r.routeErr
	}
	var out []*routing.VendorRequest
	for _, v := range vendorIDs {
		id := uuid.New()
		ref := fmt.Sprintf("MM-%06d", rand.Intn(1000000))
		res, err := r.db.ExecContext(ctx, `INSERT INTO vendor_requests (id, buyer_request_id, vendor_id, public_reference, status, sent_at)
			VALUES ($1, $2, $3, $4, 'SENT', now()) ON CONFLICT DO NOTHING`, id, req.ID, v, ref)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			out = append(out, &routing.VendorRequest{ID: id, BuyerRequestID: req.ID, VendorID: v, PublicReference: ref, Status: routing.VendorRequestSent})
		}
	}
	return out, nil
}

func (r *fakeRouter) ResendVendorRequest(ctx context.Context, id uuid.UUID) error {
	atomic.AddInt32(&r.resendCall, 1)
	if r.resend != nil {
		if err := r.resend(); err != nil {
			_, _ = r.db.ExecContext(ctx, `UPDATE vendor_requests SET status = 'SEND_FAILED', send_error = $2 WHERE id = $1`, id, err.Error())
			return err
		}
	}
	_, err := r.db.ExecContext(ctx, `UPDATE vendor_requests SET status = 'SENT', sent_at = now() WHERE id = $1 AND status = 'SEND_FAILED'`, id)
	return err
}

// fakeRequests implements workflow.Requests; only ClarifyAs is exercised
// by recovery (the broaden question).
type fakeRequests struct {
	db   *sql.DB
	msgr *fakeMessenger
}

func (f *fakeRequests) Create(context.Context, request.NewRequest) (*request.BuyerRequest, bool, error) {
	return nil, false, errors.New("unused")
}
func (f *fakeRequests) ConfirmAs(context.Context, uuid.UUID, event.ActorType, string) (*request.BuyerRequest, error) {
	return nil, errors.New("unused")
}
func (f *fakeRequests) ApplyClarification(context.Context, uuid.UUID, request.Fields, map[string]any) (*request.BuyerRequest, error) {
	return nil, errors.New("unused")
}
func (f *fakeRequests) AwaitBuyer(context.Context, uuid.UUID) (*request.BuyerRequest, error) {
	return nil, nil
}
func (f *fakeRequests) RunExtraction(context.Context, uuid.UUID) {}
func (f *fakeRequests) ClarifyAs(ctx context.Context, id uuid.UUID, q string, _ event.ActorType, extra map[string]any) error {
	var to string
	if err := f.db.QueryRowContext(ctx, `SELECT c.external_id FROM buyer_requests br JOIN conversations c ON c.id = br.conversation_id WHERE br.id = $1`, id).Scan(&to); err != nil {
		return err
	}
	return f.msgr.SendText(conversation.WithOutboundPurpose(ctx, conversation.OutboundPurpose{Kind: "clarification", Extra: extra}), to, q)
}

type noExtractor struct{}

func (noExtractor) Extract(context.Context, ai.ExtractionRequest) (*ai.ExtractionResult, error) {
	return &ai.ExtractionResult{}, nil
}

// ---- harness -----------------------------------------------------------------------------

type hardeningEnv struct {
	db     *sql.DB
	engine *workflow.Engine
	worker *jobs.Worker
	msgr   *fakeMessenger
	router *fakeRouter
	s      timeoutSeed
	cfg    workflow.Config
}

type timeoutSeed struct {
	buyer, conv, req, vendor, vr uuid.UUID
	phone                        string
}

// seedTimeout: a confirmed request for "iphone 13" in Lagos whose one
// vendor request is in vrStatus, sent sentAgo ago.
func seedTimeout(t *testing.T, db *sql.DB, phone, vrStatus string, sentAgo time.Duration) timeoutSeed {
	t.Helper()
	s := timeoutSeed{buyer: uuid.New(), conv: uuid.New(), req: uuid.New(), vendor: uuid.New(), vr: uuid.New(), phone: phone}
	mustExec(t, db, `INSERT INTO buyers (id, external_user_id) VALUES ($1, $2)`, s.buyer, phone)
	mustExec(t, db, `INSERT INTO conversations (id, external_id, buyer_id) VALUES ($1, $2, $3)`, s.conv, phone, s.buyer)
	mustExec(t, db, `INSERT INTO buyer_requests (id, conversation_id, buyer_id, status, category, product, variant, location, raw_message, confirmed_at, created_at)
		VALUES ($1, $2, $3, 'CONFIRMED', 'electronics', 'iphone 13', '128GB', 'Lagos', 'I need iPhone 13 in Lagos', now(), now() - interval '4 days')`, s.req, s.conv, s.buyer)
	mustExec(t, db, `INSERT INTO vendors (id, business_name, whatsapp_external_id, status) VALUES ($1, 'Quiet Gadgets', $2, 'ACTIVE')`, s.vendor, "v"+phone)
	mustExec(t, db, `INSERT INTO vendor_requests (id, buyer_request_id, vendor_id, public_reference, status, sent_at, created_at)
		VALUES ($1, $2, $3, $4, $5, now() - make_interval(secs => $6), now() - make_interval(secs => $6))`,
		s.vr, s.req, s.vendor, "MM-"+phone[len(phone)-6:], vrStatus, sentAgo.Seconds())
	return s
}

func addVendor(t *testing.T, db *sql.DB, name, phone string) *vendor.Vendor {
	t.Helper()
	v := &vendor.Vendor{ID: uuid.New(), BusinessName: name, WhatsAppExternalID: phone, Status: vendor.StatusActive}
	mustExec(t, db, `INSERT INTO vendors (id, business_name, whatsapp_external_id, status) VALUES ($1, $2, $3, 'ACTIVE')`, v.ID, name, phone)
	return v
}

func local(v *vendor.Vendor) matching.Candidate {
	return matching.Candidate{Vendor: v, Score: 80, Breakdown: map[string]int{"location": 20, "product": 40}}
}

func nonLocal(v *vendor.Vendor) matching.Candidate {
	return matching.Candidate{Vendor: v, Score: 80, Breakdown: map[string]int{"location": 0, "product": 40}}
}

func newHardeningEnv(t *testing.T, phone, vrStatus string, sentAgo time.Duration, mut func(*workflow.Config)) *hardeningEnv {
	t.Helper()
	db := testDB(t, migrationsPath())
	env := &hardeningEnv{db: db, msgr: &fakeMessenger{}, s: seedTimeout(t, db, phone, vrStatus, sentAgo)}
	env.router = &fakeRouter{db: db}
	env.cfg = workflow.Config{Flags: autonomy.AllOn(), Params: autonomy.DefaultParams(), JobMaxAttempts: 2}
	if mut != nil {
		mut(&env.cfg)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	auto, wf, rr := NewAutonomyRepository(db), NewWorkflowRepository(db), NewRoutingRepository(db)
	jobStore := NewJobRepository(db)
	env.engine = workflow.New(env.cfg, workflow.Deps{Store: auto, Context: wf, Requests: &fakeRequests{db: db, msgr: env.msgr},
		ReqRepo: NewRequestRepository(db), Router: env.router, Routes: rr, Extractor: noExtractor{}, Messenger: env.msgr,
		Convs: NewConversationRepository(db), Jobs: jobStore, Events: NewEventWriter(db), Logger: logger,
		Async:          func(f func(context.Context)) { f(context.Background()) },
		VendorRequests: rr, Recovery: wf, Notices: auto, Holds: auto, OfferReverter: NewOfferRepository(db)})
	env.worker = jobs.NewWorker(jobStore, jobs.WorkerConfig{PollInterval: 10 * time.Millisecond, Lease: time.Minute, Concurrency: 1,
		Retry: jobs.RetryPolicy{BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}}, logger)
	env.engine.Register(env.worker)
	return env
}

// drain runs due jobs until none is left (bounded).
func (e *hardeningEnv) drain(t *testing.T) {
	t.Helper()
	for i := 0; i < 50; i++ {
		ran, err := e.worker.RunOnce(context.Background(), "test-worker")
		if err != nil {
			t.Fatalf("worker: %v", err)
		}
		if !ran {
			time.Sleep(15 * time.Millisecond) // backoff of retried jobs
			if ran2, _ := e.worker.RunOnce(context.Background(), "test-worker"); !ran2 {
				return
			}
		}
	}
}

func (e *hardeningEnv) vrStatus(t *testing.T, id uuid.UUID) string {
	var st string
	if err := e.db.QueryRow(`SELECT status FROM vendor_requests WHERE id = $1`, id).Scan(&st); err != nil {
		t.Fatal(err)
	}
	return st
}

func (e *hardeningEnv) count(t *testing.T, q string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func (e *hardeningEnv) recoveries(t *testing.T) map[string]int {
	t.Helper()
	rows, err := e.db.Query(`SELECT outcome || ':' || reason FROM workflow_decisions WHERE buyer_request_id = $1 AND decision = 'RECOVER_REQUEST'`, e.s.req)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var k string
		_ = rows.Scan(&k)
		out[k]++
	}
	return out
}

const overdue = 80 * time.Hour // past the default 72h VENDOR_REQUEST_TTL

// ---- vendor-request deadline and recovery ------------------------------------------------

func TestHardening_TimeoutReroutesToEligibleAlternative(t *testing.T) {
	env := newHardeningEnv(t, "2348070000001", "SENT", overdue, nil)
	v2 := addVendor(t, env.db, "Alt Gadgets", "2348170000001")
	env.router.cands = func() ([]matching.Candidate, error) {
		return []matching.Candidate{local(&vendor.Vendor{ID: env.s.vendor}), local(v2)}, nil // the quiet vendor is matched again: excluded
	}
	if err := env.engine.HandleVendorTimeout(context.Background(), env.s.vr, "deadline"); err != nil {
		t.Fatal(err)
	}
	if st := env.vrStatus(t, env.s.vr); st != "EXPIRED" {
		t.Fatalf("the quiet request expires, got %s", st)
	}
	if n := env.count(t, `SELECT count(*) FROM vendor_requests WHERE buyer_request_id = $1 AND vendor_id = $2 AND status = 'SENT'`, env.s.req, v2.ID); n != 1 {
		t.Fatalf("the alternative vendor is contacted once, got %d", n)
	}
	if n := env.count(t, `SELECT count(*) FROM vendor_requests WHERE buyer_request_id = $1`, env.s.req); n != 2 {
		t.Fatalf("the quiet vendor is never asked twice, got %d requests", n)
	}
	if r := env.recoveries(t); r["APPLIED:"+autonomy.RecoverRerouted] != 1 {
		t.Fatalf("recovery recorded: %v", r)
	}
	ups := env.msgr.byKind("routing_update")
	if len(ups) != 1 || !strings.Contains(ups[0].body, "checking with 1 other seller") || strings.Contains(strings.ToLower(ups[0].body), "unavailable") {
		t.Fatalf("buyer told honestly, got %+v", ups)
	}
	if n := env.count(t, `SELECT count(*) FROM events WHERE event_type = 'VENDOR_REQUEST_EXPIRED' AND entity_id = $1`, env.s.vr); n != 1 {
		t.Fatalf("one expiry event, got %d", n)
	}
	var st string
	_ = env.db.QueryRow(`SELECT status FROM buyer_requests WHERE id = $1`, env.s.req).Scan(&st)
	if st != "CONFIRMED" {
		t.Fatalf("the buyer request stays open, got %s", st)
	}
}

func TestHardening_TimeoutNoAlternativeTellsBuyerAndKeepsRequestOpen(t *testing.T) {
	env := newHardeningEnv(t, "2348070000002", "SENT", overdue, nil)
	if err := env.engine.HandleVendorTimeout(context.Background(), env.s.vr, "deadline"); err != nil {
		t.Fatal(err)
	}
	notes := env.msgr.byKind(workflow.KindVendorTimeoutNotice)
	if len(notes) != 1 {
		t.Fatalf("buyer told once, got %d", len(notes))
	}
	body := strings.ToLower(notes[0].body)
	if strings.Contains(body, "unavailable") || strings.Contains(body, "not available") || !strings.Contains(body, "pass on any reply") {
		t.Fatalf("never declared unavailable; late replies still count: %q", notes[0].body)
	}
	if r := env.recoveries(t); r["APPLIED:"+autonomy.RecoverBuyerTold] != 1 {
		t.Fatalf("recorded: %v", r)
	}
	if env.router.routeCall != 0 {
		t.Fatalf("nobody contacted")
	}
}

func TestHardening_TimeoutProcessedTwiceAndByTwoWorkers(t *testing.T) {
	env := newHardeningEnv(t, "2348070000003", "SENT", overdue, nil)
	v2 := addVendor(t, env.db, "Alt Two", "2348170000003")
	env.router.cands = func() ([]matching.Candidate, error) { return []matching.Candidate{local(v2)}, nil }
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for _, trig := range []string{"deadline", "expired_on_vendor_message", "deadline", "sweep_sent"} {
		wg.Add(1)
		go func(tr string) {
			defer wg.Done()
			errs <- env.engine.HandleVendorTimeout(ctx, env.s.vr, tr)
		}(trig)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent run: %v", err)
		}
	}
	// And once more, later (a retried job).
	if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
		t.Fatal(err)
	}
	if n := env.count(t, `SELECT count(*) FROM workflow_decisions WHERE subject_id = $1 AND decision = 'VENDOR_REQUEST_TIMEOUT' AND reason = $2`,
		env.s.vr, autonomy.TimeoutExpired); n != 1 {
		t.Fatalf("expired exactly once, got %d", n)
	}
	if n := env.count(t, `SELECT count(*) FROM vendor_requests WHERE buyer_request_id = $1 AND vendor_id = $2`, env.s.req, v2.ID); n != 1 {
		t.Fatalf("the alternative is contacted exactly once, got %d", n)
	}
	r := env.recoveries(t)
	if r["APPLIED:"+autonomy.RecoverRerouted] != 1 {
		t.Fatalf("one recovery applied, got %v", r)
	}
	if len(env.msgr.byKind("routing_update")) != 1 {
		t.Fatalf("the buyer is told once, got %d", len(env.msgr.byKind("routing_update")))
	}
}

func TestHardening_RepeatedSilenceIsHandledOncePerState(t *testing.T) {
	env := newHardeningEnv(t, "2348070000013", "SENT", overdue, nil)
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, fmt.Sprintf("sweep_%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	r := env.recoveries(t)
	if r["APPLIED:"+autonomy.RecoverBuyerTold] != 1 || r["REJECTED:"+autonomy.RecoverDuplicate] != 2 {
		t.Fatalf("handled once, duplicates prevented and recorded: %v", r)
	}
	if len(env.msgr.byKind(workflow.KindVendorTimeoutNotice)) != 1 {
		t.Fatalf("the buyer is told once")
	}
}

func TestHardening_ReplyJustBeforeDeadlineWins(t *testing.T) {
	env := newHardeningEnv(t, "2348070000004", "SENT", 71*time.Hour, nil)
	ctx := context.Background()
	// Not due yet: nothing changes (the deadline is re-scheduled).
	if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
		t.Fatal(err)
	}
	if st := env.vrStatus(t, env.s.vr); st != "SENT" {
		t.Fatalf("not due: still awaiting, got %s", st)
	}
	if n := env.count(t, `SELECT count(*) FROM jobs WHERE job_type = 'VENDOR_REQUEST_TIMEOUT' AND run_at > now() + interval '30 minutes'`); n != 1 {
		t.Fatalf("deadline re-scheduled for later, got %d", n)
	}
	// The vendor replies (usable), then the deadline passes.
	mustExec(t, env.db, `UPDATE vendor_requests SET status = 'RESPONDED' WHERE id = $1`, env.s.vr)
	mustExec(t, env.db, `INSERT INTO vendor_responses (id, vendor_request_id, vendor_id, status, review_state, price, raw_message)
		VALUES ($1, $2, $3, 'AVAILABLE', 'REVIEWED', 610000, 'Yes 610k')`, uuid.New(), env.s.vr, env.s.vendor)
	mustExec(t, env.db, `UPDATE vendor_requests SET sent_at = now() - interval '80 hours' WHERE id = $1`, env.s.vr)
	if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
		t.Fatal(err)
	}
	if st := env.vrStatus(t, env.s.vr); st != "RESPONDED" {
		t.Fatalf("the reply wins over the expiry, got %s", st)
	}
	if n := env.count(t, `SELECT count(*) FROM workflow_decisions WHERE subject_id = $1 AND reason = $2`, env.s.vr, autonomy.TimeoutAlreadyResponded); n != 1 {
		t.Fatalf("recorded as already responded, got %d", n)
	}
	if len(env.msgr.sent) != 0 || len(env.recoveries(t)) != 0 {
		t.Fatalf("no recovery, no message: %+v", env.msgr.sent)
	}
}

func TestHardening_ReplyAfterDeadlineStillCounts(t *testing.T) {
	env := newHardeningEnv(t, "2348070000005", "SENT", overdue, nil)
	ctx := context.Background()
	if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
		t.Fatal(err)
	}
	// A late reply attached to the expired request (the routing path sets RESPONDED).
	rr := NewRoutingRepository(env.db)
	resp, err := rr.CreateVendorResponse(ctx, &routing.VendorResponse{VendorRequestID: &env.s.vr, VendorID: env.s.vendor, Status: routing.ResponseAvailable,
		ReviewState: routing.ReviewReviewed, RawMessage: "Sorry for the delay, I have it 600k", ExtractionMethod: routing.ExtractionHeuristic})
	if err != nil || resp == nil {
		t.Fatalf("late reply: %v", err)
	}
	if st := env.vrStatus(t, env.s.vr); st != "RESPONDED" {
		t.Fatalf("a late reply moves the request to RESPONDED, got %s", st)
	}
	// Another recovery trigger now finds the buyer has an option.
	if err := env.engine.RecoverRequest(ctx, env.s.req, "test", nil); err != nil {
		t.Fatal(err)
	}
	if r := env.recoveries(t); r["REJECTED:"+autonomy.RecoverHasOptions] != 1 {
		t.Fatalf("no further recovery once an option exists: %v", r)
	}
}

func TestHardening_CancelledBeforeTimeout(t *testing.T) {
	env := newHardeningEnv(t, "2348070000006", "CANCELLED", overdue, nil)
	if err := env.engine.HandleVendorTimeout(context.Background(), env.s.vr, "deadline"); err != nil {
		t.Fatal(err)
	}
	if st := env.vrStatus(t, env.s.vr); st != "CANCELLED" {
		t.Fatalf("cancelled stays cancelled (not expired), got %s", st)
	}
	if n := env.count(t, `SELECT count(*) FROM workflow_decisions WHERE subject_id = $1 AND reason = $2`, env.s.vr, autonomy.TimeoutCancelled); n != 1 {
		t.Fatalf("recorded, got %d", n)
	}
	if len(env.msgr.sent) != 0 || len(env.recoveries(t)) != 0 {
		t.Fatalf("nothing else happens")
	}
}

func TestHardening_AlreadyFulfilledRequestIsNotRecovered(t *testing.T) {
	env := newHardeningEnv(t, "2348070000007", "SENT", overdue, nil)
	v2 := addVendor(t, env.db, "Alt Seven", "2348170000007")
	env.router.cands = func() ([]matching.Candidate, error) { return []matching.Candidate{local(v2)}, nil }
	// Another vendor's offer was already selected.
	other := uuid.New()
	mustExec(t, env.db, `INSERT INTO vendor_requests (id, buyer_request_id, vendor_id, public_reference, status, sent_at) VALUES ($1, $2, $3, 'MM-777001', 'RESPONDED', now())`,
		other, env.s.req, v2.ID)
	resp := uuid.New()
	mustExec(t, env.db, `INSERT INTO vendor_responses (id, vendor_request_id, vendor_id, status, review_state, price, raw_message) VALUES ($1, $2, $3, 'AVAILABLE', 'REVIEWED', 600000, 'yes')`,
		resp, other, v2.ID)
	offerID := uuid.New()
	mustExec(t, env.db, `INSERT INTO offers (id, buyer_request_id, vendor_request_id, vendor_response_id, vendor_id, type, status, price, ranking_score)
		VALUES ($1, $2, $3, $4, $5, 'EXACT', 'ACCEPTED', 600000, 50)`, offerID, env.s.req, other, resp, v2.ID)
	mustExec(t, env.db, `INSERT INTO buyer_selections (buyer_request_id, offer_id, buyer_id) VALUES ($1, $2, $3)`, env.s.req, offerID, env.s.buyer)
	if err := env.engine.HandleVendorTimeout(context.Background(), env.s.vr, "deadline"); err != nil {
		t.Fatal(err)
	}
	if r := env.recoveries(t); r["REJECTED:"+autonomy.RecoverFulfilled] != 1 {
		t.Fatalf("fulfilled: %v", r)
	}
	if env.router.routeCall != 0 || len(env.msgr.sent) != 0 {
		t.Fatalf("nobody contacted, nothing sent")
	}
}

func TestHardening_BroaderAreaAskedOnlyThroughPolicy(t *testing.T) {
	ctx := context.Background()
	t.Run("asks the buyer before contacting other areas", func(t *testing.T) {
		env := newHardeningEnv(t, "2348070000008", "SENT", overdue, nil)
		far := addVendor(t, env.db, "Abuja Gadgets", "2348170000008")
		env.router.cands = func() ([]matching.Candidate, error) { return []matching.Candidate{nonLocal(far)}, nil }
		if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
			t.Fatal(err)
		}
		if env.router.routeCall != 0 {
			t.Fatalf("no vendor outside the area is contacted without the buyer's yes")
		}
		var kind string
		var after bool
		if err := env.db.QueryRow(`SELECT kind, (context->>'after_recovery')::boolean FROM clarifications WHERE buyer_request_id = $1 AND status = 'OPEN'`, env.s.req).Scan(&kind, &after); err != nil {
			t.Fatal(err)
		}
		if kind != autonomy.KindBroadenScope || !after {
			t.Fatalf("broaden question asked: %s %v", kind, after)
		}
		if r := env.recoveries(t); r["APPLIED:"+autonomy.RecoverBroadenAsked] != 1 {
			t.Fatalf("recorded: %v", r)
		}
	})
	t.Run("already broadened earlier: contacted directly", func(t *testing.T) {
		env := newHardeningEnv(t, "2348070000009", "SENT", overdue, nil)
		far := addVendor(t, env.db, "Kano Gadgets", "2348170000009")
		env.router.cands = func() ([]matching.Candidate, error) { return []matching.Candidate{nonLocal(far)}, nil }
		mustExec(t, env.db, `INSERT INTO workflow_decisions (workflow_type, subject_type, subject_id, buyer_request_id, decision, outcome, reason, policy_version)
			VALUES ('buyer_request', 'buyer_request', $1, $1, 'ROUTE_REQUEST', 'APPLIED', 'routing_policy_broadened', 'autonomy-v1')`, env.s.req)
		if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
			t.Fatal(err)
		}
		if n := env.count(t, `SELECT count(*) FROM vendor_requests WHERE vendor_id = $1`, far.ID); n != 1 {
			t.Fatalf("the buyer already agreed to other areas: contacted, got %d", n)
		}
	})
	t.Run("category excluded from broader search", func(t *testing.T) {
		env := newHardeningEnv(t, "2348070000010", "SENT", overdue, func(c *workflow.Config) {
			c.Params.BroadenExcludedCategories = []string{"electronics"}
		})
		far := addVendor(t, env.db, "Far Gadgets", "2348170000010")
		env.router.cands = func() ([]matching.Candidate, error) { return []matching.Candidate{nonLocal(far)}, nil }
		if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
			t.Fatal(err)
		}
		if env.count(t, `SELECT count(*) FROM clarifications WHERE buyer_request_id = $1`, env.s.req) != 0 || env.router.routeCall != 0 {
			t.Fatalf("no broader search for an excluded category")
		}
		if r := env.recoveries(t); r["APPLIED:"+autonomy.RecoverBuyerTold] != 1 {
			t.Fatalf("buyer told instead: %v", r)
		}
	})
	t.Run("buyer declined a broader search before", func(t *testing.T) {
		env := newHardeningEnv(t, "2348070000011", "SENT", overdue, nil)
		far := addVendor(t, env.db, "Far Two", "2348170000011")
		env.router.cands = func() ([]matching.Candidate, error) { return []matching.Candidate{nonLocal(far)}, nil }
		mustExec(t, env.db, `INSERT INTO workflow_decisions (workflow_type, subject_type, subject_id, buyer_request_id, decision, outcome, reason, policy_version)
			VALUES ('buyer_request', 'buyer_request', $1, $1, 'NO_MATCH', 'APPLIED', 'buyer_declined_broader_search', 'autonomy-v1')`, env.s.req)
		if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
			t.Fatal(err)
		}
		if env.count(t, `SELECT count(*) FROM clarifications WHERE buyer_request_id = $1`, env.s.req) != 0 {
			t.Fatalf("never asked again after a no")
		}
	})
}

func TestHardening_ContactLimitAndMaxRounds(t *testing.T) {
	ctx := context.Background()
	t.Run("contact limit", func(t *testing.T) {
		env := newHardeningEnv(t, "2348070000012", "SENT", overdue, func(c *workflow.Config) { c.Params.MaxVendorsPerRequest = 3 })
		for i := 0; i < 2; i++ {
			v := addVendor(t, env.db, fmt.Sprintf("Old %d", i), fmt.Sprintf("23481700001%02d", i))
			mustExec(t, env.db, `INSERT INTO vendor_requests (id, buyer_request_id, vendor_id, public_reference, status, sent_at) VALUES ($1, $2, $3, $4, 'EXPIRED', now() - interval '90 hours')`,
				uuid.New(), env.s.req, v.ID, fmt.Sprintf("MM-9900%02d", i))
		}
		fresh := addVendor(t, env.db, "Fresh", "2348170000199")
		env.router.cands = func() ([]matching.Candidate, error) { return []matching.Candidate{local(fresh)}, nil }
		if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
			t.Fatal(err)
		}
		if env.router.routeCall != 0 {
			t.Fatalf("3 vendors already contacted: no more")
		}
		if r := env.recoveries(t); r["APPLIED:"+autonomy.RecoverContactLimit] != 1 {
			t.Fatalf("recorded: %v", r)
		}
		if len(env.msgr.byKind(workflow.KindVendorTimeoutNotice)) != 1 {
			t.Fatalf("buyer told")
		}
	})
	t.Run("maximum recovery rounds", func(t *testing.T) {
		env := newHardeningEnv(t, "2348070000014", "SENT", overdue, func(c *workflow.Config) { c.Params.RecoveryMaxRounds = 1 })
		v2 := addVendor(t, env.db, "Round One", "2348170000214")
		v3 := addVendor(t, env.db, "Round Two", "2348170000314")
		env.router.cands = func() ([]matching.Candidate, error) { return []matching.Candidate{local(v2), local(v3)}, nil }
		if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
			t.Fatal(err)
		}
		// Round one went to both; they go quiet too.
		mustExec(t, env.db, `UPDATE vendor_requests SET sent_at = now() - interval '80 hours' WHERE buyer_request_id = $1 AND status = 'SENT'`, env.s.req)
		var ids []uuid.UUID
		rows, _ := env.db.Query(`SELECT id FROM vendor_requests WHERE buyer_request_id = $1 AND status = 'SENT'`, env.s.req)
		for rows.Next() {
			var id uuid.UUID
			_ = rows.Scan(&id)
			ids = append(ids, id)
		}
		rows.Close()
		v4 := addVendor(t, env.db, "Round Three", "2348170000414")
		env.router.cands = func() ([]matching.Candidate, error) { return []matching.Candidate{local(v4)}, nil }
		for _, id := range ids {
			if err := env.engine.HandleVendorTimeout(ctx, id, "deadline"); err != nil {
				t.Fatal(err)
			}
		}
		if n := env.count(t, `SELECT count(*) FROM vendor_requests WHERE vendor_id = $1`, v4.ID); n != 0 {
			t.Fatalf("no second round, got %d", n)
		}
		r := env.recoveries(t)
		if r["APPLIED:"+autonomy.RecoverRerouted] != 1 || r["APPLIED:"+autonomy.RecoverMaxRounds] != 1 || r["DEFERRED:"+autonomy.RecoverAwaiting] != 1 {
			t.Fatalf("one round, then the buyer is told (the first quiet vendor waited for the second): %v", r)
		}
	})
}

func TestHardening_RequestNoLongerValid(t *testing.T) {
	ctx := context.Background()
	t.Run("reopened for review", func(t *testing.T) {
		env := newHardeningEnv(t, "2348070000015", "SENT", overdue, nil)
		mustExec(t, env.db, `UPDATE buyer_requests SET status = 'NEEDS_REVIEW' WHERE id = $1`, env.s.req)
		if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
			t.Fatal(err)
		}
		if st := env.vrStatus(t, env.s.vr); st != "EXPIRED" {
			t.Fatalf("still expires, got %s", st)
		}
		if r := env.recoveries(t); r["REJECTED:"+autonomy.RecoverNotActive] != 1 || len(env.msgr.sent) != 0 {
			t.Fatalf("no recovery for an inactive request: %v", r)
		}
	})
	t.Run("superseded by a newer request for the same product", func(t *testing.T) {
		env := newHardeningEnv(t, "2348070000016", "SENT", overdue, nil)
		mustExec(t, env.db, `INSERT INTO buyer_requests (id, conversation_id, buyer_id, status, product, raw_message, confirmed_at) VALUES ($1, $2, $3, 'CONFIRMED', 'iPhone 13', 'iphone 13 abeg', now())`,
			uuid.New(), env.s.conv, env.s.buyer)
		if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
			t.Fatal(err)
		}
		if r := env.recoveries(t); r["REJECTED:"+autonomy.RecoverSuperseded] != 1 || len(env.msgr.sent) != 0 {
			t.Fatalf("superseded: %v", r)
		}
	})
}

func TestHardening_BuyerNotificationBlockedByWindowIsQueuedThenDelivered(t *testing.T) {
	env := newHardeningEnv(t, "2348070000017", "SENT", overdue, nil)
	ctx := context.Background()
	windowClosed := &channel.ProviderError{Provider: "whatsapp", HTTPStatus: 400, Code: 131047, Body: `{"error":{"code":131047}}`}
	env.msgr.fail = func(to, body, kind string) error { return windowClosed }
	if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
		t.Fatal(err)
	}
	var status, class string
	var code int
	if err := env.db.QueryRow(`SELECT status, failure_class, failure_code FROM pending_notices WHERE buyer_request_id = $1`, env.s.req).Scan(&status, &class, &code); err != nil {
		t.Fatal(err)
	}
	if status != "PENDING" || class != string(channel.FailPolicy) || code != 131047 {
		t.Fatalf("queued for the buyer's next message: %s %s %d", status, class, code)
	}
	if n := env.count(t, `SELECT count(*) FROM workflow_exceptions WHERE status = 'OPEN'`); n != 0 {
		t.Fatalf("a compliant later route exists: no operator exception, got %d", n)
	}
	if n := env.count(t, `SELECT count(*) FROM workflow_decisions WHERE buyer_request_id = $1 AND decision = 'NOTIFY_BUYER' AND reason = 'queued_until_buyer_writes'
		AND result->>'template_configured' = 'false'`, env.s.req); n != 1 {
		t.Fatalf("blocked communication recorded (no template assumed), got %d", n)
	}
	var st string
	_ = env.db.QueryRow(`SELECT status FROM buyer_requests WHERE id = $1`, env.s.req).Scan(&st)
	if st != "CONFIRMED" {
		t.Fatalf("business state unchanged, got %s", st)
	}
	// No free-form retry while the window is closed: a resend job is not scheduled.
	if n := env.count(t, `SELECT count(*) FROM jobs WHERE job_type = 'BUYER_NOTICE_RESEND'`); n != 0 {
		t.Fatalf("no retry of a policy-blocked message, got %d", n)
	}
	// The buyer writes two hours later: the window is open; the notice is
	// delivered once, labelled as an earlier update.
	mustExec(t, env.db, `UPDATE pending_notices SET created_at = now() - interval '2 hours' WHERE buyer_request_id = $1`, env.s.req)
	env.msgr.fail = nil
	env.engine.BuyerWrote(ctx, env.s.conv, uuid.New())
	env.drain(t)
	got := env.msgr.byKind(workflow.KindVendorTimeoutNotice)
	if len(got) != 1 || !strings.HasPrefix(got[0].body, "Earlier update:") {
		t.Fatalf("delivered late, labelled: %+v", got)
	}
	env.engine.BuyerWrote(ctx, env.s.conv, uuid.New())
	env.drain(t)
	if len(env.msgr.byKind(workflow.KindVendorTimeoutNotice)) != 1 {
		t.Fatalf("delivered exactly once")
	}
}

func TestHardening_RecoveryStepFailureIsRetriedThenRaised(t *testing.T) {
	env := newHardeningEnv(t, "2348070000018", "SENT", overdue, nil)
	env.router.cands = func() ([]matching.Candidate, error) { return nil, errors.New("matching unavailable") }
	ctx := context.Background()
	env.engine.VendorRequestExpired(ctx, &routing.VendorRequest{ID: env.s.vr})
	env.drain(t)
	if st := env.vrStatus(t, env.s.vr); st != "EXPIRED" {
		t.Fatalf("expiry is not undone by a later failure, got %s", st)
	}
	if n := env.count(t, `SELECT count(*) FROM workflow_decisions WHERE buyer_request_id = $1 AND decision = 'RECOVER_REQUEST' AND outcome = 'FAILED'`, env.s.req); n != 2 {
		t.Fatalf("each attempt recorded (JobMaxAttempts=2), got %d", n)
	}
	if n := env.count(t, `SELECT count(*) FROM workflow_exceptions WHERE buyer_request_id = $1 AND reason = $2 AND status = 'OPEN'`, env.s.req, autonomy.ReasonRecoveryFailed); n != 1 {
		t.Fatalf("raised once after the bounded retries, got %d", n)
	}
	if len(env.msgr.sent) != 0 {
		t.Fatalf("nothing sent twice or half-way")
	}
}

func TestHardening_HoldStopsRecoveryAndResumeContinues(t *testing.T) {
	env := newHardeningEnv(t, "2348070000019", "SENT", overdue, nil)
	v2 := addVendor(t, env.db, "After Hold", "2348170000019")
	env.router.cands = func() ([]matching.Candidate, error) { return []matching.Candidate{local(v2)}, nil }
	ctx := context.Background()
	if _, _, err := env.engine.Pause(ctx, env.s.req, "operator", ""); !errors.Is(err, workflow.ErrHoldReasonRequired) {
		t.Fatalf("a reason is required, got %v", err)
	}
	if _, created, err := env.engine.Pause(ctx, env.s.req, "operator", "buyer called about a dispute"); err != nil || !created {
		t.Fatalf("pause: %v", err)
	}
	if _, created, _ := env.engine.Pause(ctx, env.s.req, "operator", "again"); created {
		t.Fatalf("one active hold")
	}
	if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
		t.Fatal(err)
	}
	if env.router.routeCall != 0 || len(env.msgr.sent) != 0 {
		t.Fatalf("no automatic step while held")
	}
	if r := env.recoveries(t); r["DEFERRED:"+autonomy.RecoverHeld] != 1 {
		t.Fatalf("recorded: %v", r)
	}
	if ok, err := env.engine.Resume(ctx, env.s.req, "operator", "dispute settled"); err != nil || !ok {
		t.Fatalf("resume: %v", err)
	}
	env.drain(t)
	if n := env.count(t, `SELECT count(*) FROM vendor_requests WHERE vendor_id = $1`, v2.ID); n != 1 {
		t.Fatalf("after resume the request continues, got %d", n)
	}
	if n := env.count(t, `SELECT count(*) FROM events WHERE event_type IN ('WORKFLOW_HELD', 'WORKFLOW_RESUMED') AND entity_id = $1 AND actor_type = 'OPERATOR'`, env.s.req); n != 2 {
		t.Fatalf("pause and resume audited, got %d", n)
	}
	if ok, _ := env.engine.Resume(ctx, env.s.req, "operator", "twice"); ok {
		t.Fatalf("resuming a request that is not held reports false")
	}
}

func TestHardening_NeverSentPendingRequestIsRecovered(t *testing.T) {
	env := newHardeningEnv(t, "2348070000020", "PENDING", 30*time.Minute, nil)
	if err := env.engine.HandleVendorTimeout(context.Background(), env.s.vr, "sweep_pending"); err != nil {
		t.Fatal(err)
	}
	if st := env.vrStatus(t, env.s.vr); st != "SEND_FAILED" {
		t.Fatalf("a request stuck before its send becomes SEND_FAILED, got %s", st)
	}
	if r := env.recoveries(t); r["APPLIED:"+autonomy.RecoverBuyerTold] != 1 {
		t.Fatalf("the buyer is not left waiting: %v", r)
	}
}

func TestHardening_StartupSweepSchedulesMissingDeadlines(t *testing.T) {
	env := newHardeningEnv(t, "2348070000021", "SENT", overdue, nil)
	fresh := uuid.New()
	mustExec(t, env.db, `INSERT INTO vendor_requests (id, buyer_request_id, vendor_id, public_reference, status, sent_at) VALUES ($1, $2, $3, 'MM-210021', 'SENT', now())`,
		fresh, env.s.req, addVendor(t, env.db, "Fresh", "2348170000021").ID)
	env.engine.RecoverTimeouts(context.Background())
	if n := env.count(t, `SELECT count(*) FROM jobs WHERE job_type = 'VENDOR_REQUEST_TIMEOUT' AND payload->>'vendor_request_id' = $1 AND run_at <= now()`, env.s.vr.String()); n != 1 {
		t.Fatalf("overdue request scheduled now, got %d", n)
	}
	if n := env.count(t, `SELECT count(*) FROM jobs WHERE job_type = 'VENDOR_REQUEST_TIMEOUT' AND payload->>'vendor_request_id' = $1 AND run_at > now() + interval '70 hours'`, fresh.String()); n != 1 {
		t.Fatalf("fresh request scheduled at its deadline, got %d", n)
	}
	env.engine.RecoverTimeouts(context.Background())
	if n := env.count(t, `SELECT count(*) FROM jobs WHERE job_type = 'VENDOR_REQUEST_TIMEOUT'`); n != 2 {
		t.Fatalf("idempotent sweep, got %d jobs", n)
	}
}

// ---- delivery failures (Workstream B) ------------------------------------------------------

func graphErr(status, code int) error {
	return channel.ParseGraphError("whatsapp", status, []byte(fmt.Sprintf(`{"error":{"message":"(#%d) test","code":%d}}`, code, code)))
}

func fastRetries(c *workflow.Config) {
	c.Params.RetryFirstDelayOverride = time.Millisecond
	c.Params.RetryMaxAttempts = 3
}

func (e *hardeningEnv) exceptions(t *testing.T) map[string]int {
	t.Helper()
	rows, err := e.db.Query(`SELECT reason FROM workflow_exceptions WHERE status = 'OPEN'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		out[r]++
	}
	return out
}

func (e *hardeningEnv) failVendorRequest(t *testing.T, err error) {
	t.Helper()
	mustExec(t, e.db, `UPDATE vendor_requests SET status = 'SEND_FAILED', send_error = $2, send_failed_at = now() WHERE id = $1`, e.s.vr, err.Error())
	e.engine.VendorRequestSendFailed(context.Background(), &routing.VendorRequest{ID: e.s.vr, BuyerRequestID: e.s.req, VendorID: e.s.vendor, PublicReference: "MM-T"}, err)
}

func TestHardening_TransientVendorFailureThenSuccess(t *testing.T) {
	env := newHardeningEnv(t, "2348071000001", "SENT", time.Minute, fastRetries)
	calls := 0
	env.router.resend = func() error {
		calls++
		if calls == 1 {
			return graphErr(503, 131016)
		}
		return nil
	}
	env.failVendorRequest(t, graphErr(500, 0))
	env.drain(t)
	if st := env.vrStatus(t, env.s.vr); st != "SENT" {
		t.Fatalf("recovered by retry, got %s", st)
	}
	if n := env.count(t, `SELECT count(*) FROM workflow_decisions WHERE subject_id = $1 AND decision = 'RETRY_DELIVERY' AND reason = 'recovered'`, env.s.vr); n != 1 {
		t.Fatalf("recovery recorded, got %d", n)
	}
	if len(env.exceptions(t)) != 0 || len(env.recoveries(t)) != 0 {
		t.Fatalf("no exception and no re-routing when a retry works: %v %v", env.exceptions(t), env.recoveries(t))
	}
}

func TestHardening_VendorRetryExhaustionRaisesAndRecovers(t *testing.T) {
	env := newHardeningEnv(t, "2348071000002", "SENT", time.Minute, fastRetries)
	env.router.resend = func() error { return graphErr(503, 0) }
	env.failVendorRequest(t, graphErr(503, 0))
	env.drain(t)
	if c := atomic.LoadInt32(&env.router.resendCall); c != 3 {
		t.Fatalf("bounded: exactly AUTO_RETRY_MAX_ATTEMPTS=3 resends, got %d", c)
	}
	if ex := env.exceptions(t); ex[autonomy.ReasonDeliveryRetryExhausted] != 1 {
		t.Fatalf("exhaustion raised once: %v", ex)
	}
	if r := env.recoveries(t); r["APPLIED:"+autonomy.RecoverBuyerTold] != 1 {
		t.Fatalf("the buyer's request does not wait on an unreachable vendor: %v", r)
	}
}

func TestHardening_RateLimitIsRetriedLater(t *testing.T) {
	env := newHardeningEnv(t, "2348071000003", "SENT", time.Minute, nil)
	env.failVendorRequest(t, graphErr(400, 130429))
	if n := env.count(t, `SELECT count(*) FROM jobs WHERE job_type = 'VENDOR_REQUEST_RESEND' AND run_at > now() + interval '50 seconds'`); n != 1 {
		t.Fatalf("a throttled send (HTTP 400 + 130429) is retried after the class delay, got %d", n)
	}
	if n := env.count(t, `SELECT count(*) FROM workflow_decisions WHERE subject_id = $1 AND reason = 'retry_scheduled' AND result->>'class' = 'RATE_LIMITED'`, env.s.vr); n != 1 {
		t.Fatalf("class persisted, got %d", n)
	}
	if len(env.exceptions(t)) != 0 {
		t.Fatalf("throttling is not an operator event")
	}
}

func TestHardening_PolicyRestrictedVendorMessage(t *testing.T) {
	env := newHardeningEnv(t, "2348071000004", "SENT", time.Minute, fastRetries)
	v2 := addVendor(t, env.db, "Open Window", "2348171000004")
	env.router.cands = func() ([]matching.Candidate, error) { return []matching.Candidate{local(v2)}, nil }
	env.failVendorRequest(t, graphErr(400, 131047))
	env.drain(t)
	if n := env.count(t, `SELECT count(*) FROM jobs WHERE job_type = 'VENDOR_REQUEST_RESEND'`); n != 0 {
		t.Fatalf("a policy refusal is never retried as free text, got %d jobs", n)
	}
	if n := env.count(t, `SELECT count(*) FROM workflow_decisions WHERE subject_id = $1 AND decision = 'RETRY_DELIVERY' AND outcome = 'REJECTED'
		AND result->>'template_required' = 'true' AND result->>'template_configured' = 'false'`, env.s.vr); n != 1 {
		t.Fatalf("blocked send recorded with the missing template, got %d", n)
	}
	if ex := env.exceptions(t); ex[autonomy.ReasonProviderPolicy] != 1 {
		t.Fatalf("the operator can fix the vendor's window / template: %v", ex)
	}
	if n := env.count(t, `SELECT count(*) FROM vendor_requests WHERE vendor_id = $1`, v2.ID); n != 1 {
		t.Fatalf("meanwhile the buyer's request continues with another vendor, got %d", n)
	}
}

func TestHardening_InvalidRecipientUnknownAndAuthFailures(t *testing.T) {
	t.Run("invalid recipient", func(t *testing.T) {
		env := newHardeningEnv(t, "2348071000005", "SENT", time.Minute, fastRetries)
		env.failVendorRequest(t, graphErr(400, 131026))
		env.drain(t)
		if ex := env.exceptions(t); ex[autonomy.ReasonInvalidRecipient] != 1 {
			t.Fatalf("the stored number must be fixed: %v", ex)
		}
		if n := env.count(t, `SELECT count(*) FROM jobs WHERE job_type = 'VENDOR_REQUEST_RESEND'`); n != 0 {
			t.Fatalf("never retried")
		}
	})
	t.Run("unknown provider error", func(t *testing.T) {
		env := newHardeningEnv(t, "2348071000006", "SENT", time.Minute, fastRetries)
		env.failVendorRequest(t, graphErr(400, 987654))
		env.drain(t)
		if ex := env.exceptions(t); ex[autonomy.ReasonUnclassifiedProvider] != 1 {
			t.Fatalf("unknown errors are surfaced, not retried blindly: %v", ex)
		}
		if n := env.count(t, `SELECT count(*) FROM jobs WHERE job_type = 'VENDOR_REQUEST_RESEND'`); n != 0 {
			t.Fatalf("never retried")
		}
	})
	t.Run("auth failure raises one channel exception", func(t *testing.T) {
		env := newHardeningEnv(t, "2348071000007", "SENT", time.Minute, fastRetries)
		ctx := context.Background()
		env.failVendorRequest(t, graphErr(401, 190))
		env.engine.OfferDeliveryFailed(ctx, env.s.req, graphErr(401, 190))
		env.drain(t)
		if n := env.count(t, `SELECT count(*) FROM workflow_exceptions WHERE status = 'OPEN' AND reason = $1 AND subject_type = 'channel'`, autonomy.ReasonProviderAuthConfig); n != 1 {
			t.Fatalf("one account-level exception, got %d (%v)", n, env.exceptions(t))
		}
	})
}

func TestHardening_AsyncVendorFailureFromWebhook(t *testing.T) {
	env := newHardeningEnv(t, "2348071000008", "SENT", time.Hour, fastRetries)
	ctx := context.Background()
	f := conversation.OutboundFailure{MessageID: uuid.New(), ConversationID: env.s.conv, Kind: "vendor_request", EntityType: "vendor_request",
		EntityID: &env.s.vr, Code: 131026, Title: "Message undeliverable"}
	env.engine.OutboundFailed(ctx, f)
	env.engine.OutboundFailed(ctx, f) // the provider reports it twice
	env.drain(t)
	if st := env.vrStatus(t, env.s.vr); st != "SEND_FAILED" {
		t.Fatalf("an accepted-then-failed vendor request no longer counts as awaiting, got %s", st)
	}
	if r := env.recoveries(t); r["APPLIED:"+autonomy.RecoverBuyerTold] != 1 {
		t.Fatalf("recovered once: %v", r)
	}
	if n := env.count(t, `SELECT count(*) FROM events WHERE event_type = 'OUTBOUND_DELIVERY_FAILED_CLASSIFIED'`); n != 2 {
		t.Fatalf("each report recorded, got %d", n)
	}
}

func TestHardening_AsyncOfferFailureIsRedeliveredWhenBuyerWrites(t *testing.T) {
	env := newHardeningEnv(t, "2348071000009", "RESPONDED", time.Hour, fastRetries)
	ctx := context.Background()
	resp := uuid.New()
	mustExec(t, env.db, `INSERT INTO vendor_responses (id, vendor_request_id, vendor_id, status, review_state, price, raw_message) VALUES ($1, $2, $3, 'AVAILABLE', 'REVIEWED', 600000, 'yes')`,
		resp, env.s.vr, env.s.vendor)
	offerID := uuid.New()
	mustExec(t, env.db, `INSERT INTO offers (id, buyer_request_id, vendor_request_id, vendor_response_id, vendor_id, type, status, price, ranking_score, presentation_ordinal, sent_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, 'EXACT', 'SENT', 600000, 50, 1, now(), now() + interval '24 hours')`, offerID, env.s.req, env.s.vr, resp, env.s.vendor)
	var delivered int32
	env.engine.SetOfferDeliverer(func(ctx context.Context, id uuid.UUID) error {
		atomic.AddInt32(&delivered, 1)
		return nil
	})
	f := conversation.OutboundFailure{MessageID: uuid.New(), ConversationID: env.s.conv, Kind: "offer_delivery", EntityType: "buyer_request",
		EntityID: &env.s.req, Metadata: map[string]any{"offer_ids": []any{offerID.String()}}, Code: 131047, Title: "Re-engagement message"}
	env.engine.OutboundFailed(ctx, f)
	var st string
	var ord sql.NullInt64
	_ = env.db.QueryRow(`SELECT status, presentation_ordinal FROM offers WHERE id = $1`, offerID).Scan(&st, &ord)
	if st != "REVIEWED" || ord.Valid {
		t.Fatalf("the buyer never saw it: back to deliverable, got %s %v", st, ord)
	}
	if n := env.count(t, `SELECT count(*) FROM pending_notices WHERE buyer_request_id = $1 AND kind = 'offer_delivery' AND status = 'PENDING'`, env.s.req); n != 1 {
		t.Fatalf("offers wait for the buyer's next message, got %d", n)
	}
	if len(env.exceptions(t)) != 0 {
		t.Fatalf("a compliant route exists: no exception, got %v", env.exceptions(t))
	}
	env.engine.OutboundFailed(ctx, f) // reported twice: nothing more happens
	if n := env.count(t, `SELECT count(*) FROM pending_notices WHERE buyer_request_id = $1`, env.s.req); n != 1 {
		t.Fatalf("duplicate report deduplicated, got %d", n)
	}
	env.engine.BuyerWrote(ctx, env.s.conv, uuid.New())
	env.drain(t)
	if atomic.LoadInt32(&delivered) != 1 {
		t.Fatalf("offers delivered once when the buyer wrote, got %d", delivered)
	}
	if n := env.count(t, `SELECT count(*) FROM pending_notices WHERE buyer_request_id = $1 AND status = 'SENT'`, env.s.req); n != 1 {
		t.Fatalf("notice closed, got %d", n)
	}
}

func TestHardening_AsyncQuestionFailureIsReaskedNotLooped(t *testing.T) {
	env := newHardeningEnv(t, "2348071000010", "SENT", time.Hour, nil)
	ctx := context.Background()
	auto := NewAutonomyRepository(env.db)
	c, err := auto.OpenClarification(ctx, &autonomy.Clarification{Party: autonomy.PartyBuyer, ConversationID: &env.s.conv, BuyerID: &env.s.buyer,
		BuyerRequestID: &env.s.req, Kind: autonomy.KindMissingField, Field: "location", Question: "Which city?", ExpiresAt: time.Now().Add(time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	env.engine.OutboundFailed(ctx, conversation.OutboundFailure{MessageID: uuid.New(), ConversationID: env.s.conv, Kind: "clarification",
		EntityType: "buyer_request", EntityID: &env.s.req, Metadata: map[string]any{"clarification_id": c.ID.String()}, Code: 131000})
	var st string
	_ = env.db.QueryRow(`SELECT status FROM clarifications WHERE id = $1`, c.ID).Scan(&st)
	if st != autonomy.ClarSuperseded {
		t.Fatalf("an undelivered question is not left open (the buyer's next words are not its answer), got %s", st)
	}
	if n := env.count(t, `SELECT count(*) FROM jobs WHERE job_type = 'WORKFLOW_ADVANCE'`); n != 0 {
		t.Fatalf("no immediate re-ask loop, got %d", n)
	}
}

func TestHardening_TransientBuyerNoticeRetriedThenWaitsForBuyer(t *testing.T) {
	env := newHardeningEnv(t, "2348071000011", "SENT", overdue, fastRetries)
	env.msgr.fail = func(to, body, kind string) error { return graphErr(503, 0) }
	if err := env.engine.HandleVendorTimeout(context.Background(), env.s.vr, "deadline"); err != nil {
		t.Fatal(err)
	}
	env.drain(t)
	var status string
	var attempts int
	_ = env.db.QueryRow(`SELECT status, attempts FROM pending_notices WHERE buyer_request_id = $1`, env.s.req).Scan(&status, &attempts)
	if status != "PENDING" || attempts != 3 {
		t.Fatalf("bounded resends (3), then kept for the buyer's next message: %s %d", status, attempts)
	}
	if n := env.count(t, `SELECT count(*) FROM workflow_decisions WHERE decision = 'NOTIFY_BUYER' AND reason = 'retry_exhausted_waiting_for_buyer'`); n != 1 {
		t.Fatalf("exhaustion recorded, got %d", n)
	}
}

// ---- metrics with explicit denominators (Workstream D) -----------------------------------

func TestHardening_MetricsHaveOneLevelEachAndRawCounts(t *testing.T) {
	db := testDB(t, migrationsPath())
	ctx := context.Background()
	repo := NewAutonomyRepository(db)
	mk := func(phone, status string) timeoutSeed { return seedTimeout(t, db, phone, "SENT", time.Hour) }
	// 1. completed: a transaction.
	a := mk("2348072000001", "CONFIRMED")
	resp, offerID, sel := uuid.New(), uuid.New(), uuid.New()
	mustExec(t, db, `UPDATE vendor_requests SET status = 'RESPONDED' WHERE id = $1`, a.vr)
	mustExec(t, db, `INSERT INTO vendor_responses (id, vendor_request_id, vendor_id, status, review_state, price, raw_message) VALUES ($1, $2, $3, 'AVAILABLE', 'REVIEWED', 500000, 'yes')`, resp, a.vr, a.vendor)
	mustExec(t, db, `INSERT INTO offers (id, buyer_request_id, vendor_request_id, vendor_response_id, vendor_id, type, status, price, ranking_score) VALUES ($1, $2, $3, $4, $5, 'EXACT', 'ACCEPTED', 500000, 50)`, offerID, a.req, a.vr, resp, a.vendor)
	mustExec(t, db, `INSERT INTO buyer_selections (id, buyer_request_id, offer_id, buyer_id) VALUES ($1, $2, $3, $4)`, sel, a.req, offerID, a.buyer)
	mustExec(t, db, `INSERT INTO transactions (buyer_selection_id, buyer_request_id, offer_id, buyer_id, vendor_id, gross_amount) VALUES ($1, $2, $3, $4, $5, 500000)`, sel, a.req, offerID, a.buyer, a.vendor)
	// 2. clarified: a question answered, then the same field asked again (not "resolved").
	b := mk("2348072000002", "CONFIRMED")
	q := func(s timeoutSeed, field, status string, attempts int, ago string) {
		mustExec(t, db, `INSERT INTO clarifications (party, conversation_id, buyer_id, buyer_request_id, kind, field, question, status, attempts, asked_at, expires_at)
			VALUES ('BUYER', $1, $2, $3, 'missing_field', $4, 'q?', $5, $6, now() - $7::interval, now() + interval '1 hour')`, s.conv, s.buyer, s.req, field, status, attempts, ago)
	}
	q(b, "spec", "ANSWERED", 1, "3 hours")
	q(b, "spec", "ANSWERED", 2, "2 hours") // asked again, re-asked once more: resolved
	// 3. abandoned: waiting on the buyer, question expired.
	c := mk("2348072000003", "CONFIRMED")
	mustExec(t, db, `UPDATE buyer_requests SET status = 'AWAITING_BUYER' WHERE id = $1`, c.req)
	q(c, "location", "EXPIRED", 1, "30 hours")
	// 4. ended without offer: buyer told nobody replied.
	d := mk("2348072000004", "CONFIRMED")
	mustExec(t, db, `INSERT INTO workflow_decisions (workflow_type, subject_type, subject_id, buyer_request_id, decision, outcome, reason, policy_version)
		VALUES ('buyer_request', 'buyer_request', $1, $1, 'RECOVER_REQUEST', 'APPLIED', 'buyer_told_no_reply_yet', 'autonomy-v1')`, d.req)
	// 5. pending, escalated (a system exception) and held.
	e := mk("2348072000005", "CONFIRMED")
	if _, err := repo.RaiseException(ctx, &autonomy.Exception{Class: autonomy.SystemException, Reason: autonomy.ReasonProviderPolicy, SubjectType: "vendor_request",
		SubjectID: e.vr, BuyerRequestID: &e.req}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repo.PlaceHold(ctx, e.req, "operator", "check"); err != nil {
		t.Fatal(err)
	}
	// An undelivered question is not counted as delivered.
	mustExec(t, db, `INSERT INTO clarifications (party, conversation_id, buyer_id, buyer_request_id, kind, field, question, status, resolution, expires_at)
		VALUES ('BUYER', $1, $2, $3, 'missing_field', 'budget', 'q?', 'SUPERSEDED', '{"reason":"undelivered"}', now())`, e.conv, e.buyer, e.req)

	m, err := repo.Metrics(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	w := m.Workflow
	check := func(name string, got autonomy.Rate, n, of int) {
		t.Helper()
		if got.N != n || got.Of != of {
			t.Fatalf("%s: got %d/%d want %d/%d", name, got.N, got.Of, n, of)
		}
	}
	if w.Started != 5 {
		t.Fatalf("started %d", w.Started)
	}
	check("completed", w.Completed, 1, 5)
	check("completed autonomously", w.CompletedAutonomously, 1, 5)
	check("requiring clarification", w.RequiringClarification, 2, 5)
	check("escalated", w.EscalatedToOperator, 1, 5)
	check("held", w.Held, 1, 5)
	check("ended without offer", w.EndedWithoutOffer, 1, 5)
	check("abandoned", w.Abandoned, 1, 5)
	check("pending", w.Pending, 2, 5) // b and e
	qm := m.Questions
	if qm.Delivered != 3 || qm.MessagesSent != 4 || qm.UndeliveredClosed != 1 {
		t.Fatalf("questions: %+v", qm)
	}
	check("answered", qm.Answered, 2, 3)
	check("resolved", qm.Resolved, 1, 3)       // the first spec answer did not settle it
	check("re-asked", qm.ReAsked, 2, 3)        // first was asked again later; second has attempts 2
	check("expired", qm.ExpiredOrGaveUp, 1, 3) // c
	if m.Definitions["questions.resolved"] == "" || m.Definitions["legacy.clarification_resolution"] == "" {
		t.Fatalf("definitions served with the numbers")
	}
	if m.Delivery.SystemExceptions != 1 {
		t.Fatalf("delivery: %+v", m.Delivery)
	}
}
