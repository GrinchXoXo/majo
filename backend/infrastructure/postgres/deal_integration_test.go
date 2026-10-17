package postgres

// Pilot batch item 5: deal follow-up against real Postgres, the real
// transaction service, the real autonomy store and job store, and the real
// buyer runtime answering the questions.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/deal"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/transaction"
	"github.com/middleman/backend/internal/workflow"
)

type dealEnv struct {
	*hardeningEnv
	svc   *deal.Service
	repo  *DealRepository
	txSvc *transaction.Service
	now   time.Time
}

func newDealEnv(t *testing.T, phone string, freeDeals int) *dealEnv {
	t.Helper()
	h := newHardeningEnv(t, phone, "SENT", time.Hour, nil)
	db := h.db
	repo := NewDealRepository(db)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := deal.DefaultConfig()
	cfg.FreeDeals = freeDeals
	svc := deal.NewService(cfg, deal.Deps{Repo: repo, Offers: NewOfferRepository(db), Requests: NewRequestRepository(db),
		Vendors: NewVendorRepository(db), Conversations: NewConversationRepository(db), References: repo, Messenger: h.msgr,
		Store: NewAutonomyRepository(db), Jobs: NewJobRepository(db), Events: NewEventWriter(db), Logger: logger, Holds: h.engine})
	h.engine.SetDeals(svc)
	txSvc := transaction.NewService(NewTransactionRepository(db), NewOfferRepository(db), NewRequestRepository(db), NewEventWriter(db), NewAuditWriter(db), logger)
	txSvc.SetListener(svc)
	return &dealEnv{hardeningEnv: h, svc: svc, repo: repo, txSvc: txSvc, now: time.Now().UTC()}
}

// offerFor: an offer from the seeded vendor on the seeded request.
func (e *dealEnv) offerFor(t *testing.T, price int64) uuid.UUID {
	t.Helper()
	resp, id := uuid.New(), uuid.New()
	mustExec(t, e.db, `INSERT INTO vendor_responses (id, vendor_request_id, vendor_id, status, review_state, price, raw_message)
		VALUES ($1, $2, $3, 'AVAILABLE', 'REVIEWED', $4, 'Yes I get am')`, resp, e.s.vr, e.s.vendor, price)
	mustExec(t, e.db, `INSERT INTO offers (id, buyer_request_id, vendor_request_id, vendor_response_id, vendor_id, type, status, product, variant, price, ranking_score, sent_at)
		VALUES ($1, $2, $3, $4, $5, 'EXACT', 'SENT', 'iphone 13', '128GB', $6, 60, now())`, id, e.s.req, e.s.vr, resp, e.s.vendor, price)
	return id
}

// buyerSays: an inbound buyer message answered through the real buyer runtime.
func (e *dealEnv) buyerSays(t *testing.T, text string) {
	t.Helper()
	conv := NewConversationRepository(e.db)
	msg, err := conv.SaveMessage(context.Background(), &conversation.Message{ConversationID: e.s.conv, Direction: conversation.DirectionInbound,
		ExternalMessageID: "wamid." + uuid.NewString(), MessageType: "text", Content: map[string]any{"text": text}})
	if err != nil {
		t.Fatal(err)
	}
	e.engine.ProcessAnswer(context.Background(), workflow.BuyerText{ConversationID: e.s.conv, BuyerID: e.s.buyer, MessageID: msg.ID, Text: text})
}

func (e *dealEnv) deal(t *testing.T) *deal.Deal {
	t.Helper()
	rows, _, err := e.repo.List(context.Background(), "", 10, 0)
	if err != nil || len(rows) == 0 {
		t.Fatalf("no deal: %v", err)
	}
	return rows[0]
}

func TestIntegration_DealIntroductionFollowUpConfirmAndRate(t *testing.T) {
	e := newDealEnv(t, "2348070000051", 100)
	ctx := context.Background()
	offerID := e.offerFor(t, 620000)

	res, err := e.txSvc.Select(ctx, e.s.req, offerID, e.s.buyer, transaction.SourceWhatsApp)
	if err != nil || res.AlreadyRecorded {
		t.Fatalf("select: %v", err)
	}
	d := e.deal(t)
	if d.Status != deal.StatusPending || d.Item != "iphone 13 128GB" || d.Reference == "" || d.Amount == nil || *d.Amount != 620000 {
		t.Fatalf("deal: %+v", d)
	}
	if d.AskAt.Before(time.Now().Add(5*time.Hour)) || d.AskAt.After(time.Now().Add(7*time.Hour)) {
		t.Fatalf("asked ~6h after the choice: %v", d.AskAt)
	}
	intro := e.msgr.byKind("deal_introduction_vendor")
	if len(intro) != 1 || intro[0].to != "v2348070000051" || !strings.Contains(intro[0].body, "₦620,000") || !strings.Contains(intro[0].body, d.Reference) {
		t.Fatalf("vendor introduced: %+v", intro)
	}
	if n := len(e.msgr.byKind("deal_introduction_buyer")); n != 0 {
		t.Fatalf("a buyer who chose on WhatsApp already got the introduction in the confirmation, got %d", n)
	}
	if intro := e.svc.Introduction(ctx, offerID); !strings.Contains(intro, "Seller: Quiet Gadgets") || !strings.Contains(intro, "wa.me/") {
		t.Fatalf("buyer introduction text: %q", intro)
	}
	// Selecting again (duplicate webhook) introduces nobody twice.
	if _, err := e.txSvc.Select(ctx, e.s.req, offerID, e.s.buyer, transaction.SourceWhatsApp); err != nil {
		t.Fatal(err)
	}
	if n := len(e.msgr.byKind("deal_introduction_vendor")); n != 1 {
		t.Fatalf("one introduction, got %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM jobs WHERE job_type = 'DEAL_FOLLOWUP' AND run_at > now() + interval '5 hours'`); n != 1 {
		t.Fatalf("follow-up scheduled once, got %d", n)
	}

	// The follow-up runs.
	if err := e.svc.FollowUp(ctx, d.ID, "BOTH"); err != nil {
		t.Fatal(err)
	}
	if bq, vq := e.msgr.byKind("deal_followup_buyer"), e.msgr.byKind("deal_followup_vendor"); len(bq) != 1 || len(vq) != 1 ||
		!strings.Contains(bq[0].body, "did you buy the iphone 13 128GB from Quiet Gadgets?") || !strings.Contains(vq[0].body, d.Reference) {
		t.Fatalf("questions: %+v %+v", bq, vq)
	}
	if err := e.svc.FollowUp(ctx, d.ID, "BOTH"); err != nil { // a duplicate run asks nobody twice
		t.Fatal(err)
	}
	if n := len(e.msgr.byKind("deal_followup_buyer")) + len(e.msgr.byKind("deal_followup_vendor")); n != 2 {
		t.Fatalf("asked once each, got %d", n)
	}
	if st := e.deal(t).Status; st != deal.StatusAsked {
		t.Fatalf("ASKED, got %s", st)
	}

	// The buyer answers in Pidgin through the real buyer runtime.
	e.buyerSays(t, "I don buy am 👍")
	rq := e.msgr.byKind("deal_rating_question")
	if len(rq) != 1 || !strings.Contains(rq[0].body, "1 (bad) to 5 (great)") {
		t.Fatalf("rating asked: %+v", rq)
	}
	if st := e.deal(t).Status; st != deal.StatusAsked {
		t.Fatalf("one YES is not a deal yet, got %s", st)
	}
	// The vendor answers (the vendor runtime hands it over).
	if err := e.svc.VendorAnswered(ctx, d.ID, deal.Yes); err != nil {
		t.Fatal(err)
	}
	got := e.deal(t)
	if got.Status != deal.StatusConfirmed || got.DealNumber == nil || *got.DealNumber != 1 || !got.Free || got.ConfirmedAt == nil {
		t.Fatalf("confirmed deal #1, free: %+v", got)
	}
	// The rating: a 2 reaches the operator.
	e.buyerSays(t, "2")
	got = e.deal(t)
	if got.BuyerRating == nil || *got.BuyerRating != 2 {
		t.Fatalf("rating stored: %+v", got.BuyerRating)
	}
	if n := e.count(t, `SELECT count(*) FROM workflow_exceptions WHERE subject_type = 'deal' AND reason = 'low_rating' AND status = 'OPEN'`); n != 1 {
		t.Fatalf("low rating raised, got %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM events WHERE event_type IN ('DEAL_CREATED', 'DEAL_CONFIRMED', 'DEAL_RATED')`); n != 3 {
		t.Fatalf("events, got %d", n)
	}
	// A late answer after the deal is settled changes nothing.
	if err := e.svc.BuyerAnswered(ctx, d.ID, deal.No); err != nil {
		t.Fatal(err)
	}
	if st := e.deal(t).Status; st != deal.StatusConfirmed {
		t.Fatalf("settled: %s", st)
	}
	// The dashboard summary.
	s, err := e.svc.Summary(ctx)
	if err != nil || s.Confirmed != 1 || s.FreeUsed != 1 || s.FreeRemaining != 99 || s.Ratings != 1 || s.LowRatings != 1 ||
		s.AverageRating == nil || *s.AverageRating != 2 || s.ConfirmedValue != 620000 {
		t.Fatalf("summary: %+v %v", s, err)
	}
	// Deal questions are not request-understanding questions.
	m, err := NewAutonomyRepository(e.db).Metrics(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Workflow.RequiringClarification.N != 0 || m.Questions.Delivered != 0 || m.Workflow.EscalatedToOperator.N != 0 {
		t.Fatalf("metrics count deal follow-ups: clarified=%d questions=%d escalated=%d",
			m.Workflow.RequiringClarification.N, m.Questions.Delivered, m.Workflow.EscalatedToOperator.N)
	}
}

func TestIntegration_DealDisputeNotYetAndClose(t *testing.T) {
	e := newDealEnv(t, "2348070000052", 100)
	ctx := context.Background()
	if _, err := e.txSvc.Select(ctx, e.s.req, e.offerFor(t, 50000), e.s.buyer, transaction.SourceAPI); err != nil {
		t.Fatal(err)
	}
	if n := len(e.msgr.byKind("deal_introduction_buyer")); n != 1 {
		t.Fatalf("a choice made for the buyer (operator/API) introduces the seller to the buyer too, got %d", n)
	}
	d := e.deal(t)
	if err := e.svc.FollowUp(ctx, d.ID, "BOTH"); err != nil {
		t.Fatal(err)
	}
	e.buyerSays(t, "not yet")
	if n := e.count(t, `SELECT count(*) FROM jobs WHERE job_type = 'DEAL_FOLLOWUP' AND payload->>'party' = 'BUYER'`); n != 1 {
		t.Fatalf("not yet: re-ask scheduled, got %d", n)
	}
	if err := e.svc.FollowUp(ctx, d.ID, deal.Buyer); err != nil { // the re-ask
		t.Fatal(err)
	}
	if n := len(e.msgr.byKind("deal_followup_buyer")); n != 2 {
		t.Fatalf("buyer asked again, got %d", n)
	}
	e.buyerSays(t, "no o, e no work")
	if err := e.svc.VendorAnswered(ctx, d.ID, deal.Yes); err != nil {
		t.Fatal(err)
	}
	if st := e.deal(t).Status; st != deal.StatusDisputed {
		t.Fatalf("one NO, one YES: DISPUTED, got %s", st)
	}
	if n := e.count(t, `SELECT count(*) FROM workflow_exceptions WHERE subject_type = 'deal' AND reason = 'deal_disputed' AND class = 'SAFETY_EXCEPTION'`); n != 1 {
		t.Fatalf("dispute raised, got %d", n)
	}
	if got, _, err := e.repo.Close(ctx, d.ID, time.Now()); err != nil || got.Status != deal.StatusDisputed || got.ClosedAt == nil || got.DealNumber != nil {
		t.Fatalf("closing keeps DISPUTED, never numbered: %+v %v", got, err)
	}

	// A second request whose vendor never answers: BUYER_ONLY at close.
	e2 := newDealEnv(t, "2348070000053", 100)
	if _, err := e2.txSvc.Select(ctx, e2.s.req, e2.offerFor(t, 9000), e2.s.buyer, transaction.SourceWhatsApp); err != nil {
		t.Fatal(err)
	}
	d2 := e2.deal(t)
	_ = e2.svc.FollowUp(ctx, d2.ID, "BOTH")
	e2.buyerSays(t, "yes")
	if got, prev, err := e2.repo.Close(ctx, d2.ID, time.Now()); err != nil || prev != deal.StatusAsked || got.Status != deal.StatusBuyerOnly {
		t.Fatalf("buyer-only at close: %+v %s %v", got, prev, err)
	}
}

func TestIntegration_DealFollowUpWaitsForAnOpenQuestionAndForAPause(t *testing.T) {
	e := newDealEnv(t, "2348070000054", 100)
	ctx := context.Background()
	if _, err := e.txSvc.Select(ctx, e.s.req, e.offerFor(t, 7000), e.s.buyer, transaction.SourceWhatsApp); err != nil {
		t.Fatal(err)
	}
	d := e.deal(t)
	// Automation paused: nothing asked, retried later.
	if _, _, err := e.engine.PauseAll(ctx, "Godyes", "drill"); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.FollowUp(ctx, d.ID, "BOTH"); err != nil {
		t.Fatal(err)
	}
	if n := len(e.msgr.byKind("deal_followup_buyer")); n != 0 {
		t.Fatalf("paused: nothing asked, got %d", n)
	}
	if n := e.count(t, `SELECT count(*) FROM jobs WHERE job_type = 'DEAL_FOLLOWUP' AND idempotency_key LIKE '%retry%'`); n != 1 {
		t.Fatalf("retry scheduled, got %d", n)
	}
	_, _ = e.engine.ResumeAll(ctx, "Godyes", "done")
	// Another question is open in the buyer's conversation: the buyer side waits.
	mustExec(t, e.db, `INSERT INTO clarifications (party, conversation_id, buyer_id, kind, field, question, status, attempts, expires_at)
		VALUES ('BUYER', $1, $2, 'missing_field', 'location', 'Which area?', 'OPEN', 1, now() + interval '1 day')`, e.s.conv, e.s.buyer)
	if err := e.svc.FollowUp(ctx, d.ID, "BOTH"); err != nil {
		t.Fatal(err)
	}
	if b, v := len(e.msgr.byKind("deal_followup_buyer")), len(e.msgr.byKind("deal_followup_vendor")); b != 0 || v != 1 {
		t.Fatalf("buyer waits, vendor asked: buyer=%d vendor=%d", b, v)
	}
	if n := e.count(t, `SELECT count(*) FROM clarifications WHERE conversation_id = $1 AND kind = 'missing_field' AND status = 'OPEN'`, e.s.conv); n != 1 {
		t.Fatalf("the buyer's open question is never superseded by a deal follow-up")
	}
}

func TestIntegration_DealNumbersAreUniqueGapFreeAndFreeUpToTheLimit(t *testing.T) {
	db := testDB(t, migrationsPath())
	ctx := context.Background()
	repo := NewDealRepository(db)
	const n = 12
	ids := make([]uuid.UUID, n)
	for i := 0; i < n; i++ {
		s := seed(t, db, fmt.Sprintf("23480990%05d", i))
		offerID := insertOffer(t, db, s, &s.resp, 50, "SENT")
		tx := uuid.New()
		sel := uuid.New()
		mustExec(t, db, `INSERT INTO buyer_selections (id, buyer_request_id, offer_id, buyer_id, status, source) VALUES ($1, $2, $3, $4, 'ACTIVE', 'api')`, sel, s.req, offerID, s.buyer)
		mustExec(t, db, `INSERT INTO transactions (id, buyer_selection_id, buyer_request_id, offer_id, buyer_id, vendor_id, gross_amount) VALUES ($1, $2, $3, $4, $5, $6, 620000)`,
			tx, sel, s.req, offerID, s.buyer, s.vendor)
		d, created, err := repo.Create(ctx, &deal.Deal{TransactionID: tx, BuyerRequestID: s.req, BuyerID: s.buyer, VendorID: s.vendor,
			ConversationID: s.conv, Item: "iphone 13", AskAt: time.Now()})
		if err != nil || !created {
			t.Fatalf("create: %v", err)
		}
		if _, again, _ := repo.Create(ctx, &deal.Deal{TransactionID: tx, BuyerRequestID: s.req, BuyerID: s.buyer, VendorID: s.vendor,
			ConversationID: s.conv, Item: "x", AskAt: time.Now()}); again {
			t.Fatalf("one deal per transaction")
		}
		ids[i] = d.ID
		if _, _, _, err := repo.RecordAnswer(ctx, d.ID, deal.Buyer, deal.Yes, time.Now(), 5); err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id uuid.UUID) {
			defer wg.Done()
			if _, _, _, err := repo.RecordAnswer(ctx, id, deal.Vendor, deal.Yes, time.Now(), 5); err != nil {
				t.Error(err)
			}
		}(id)
	}
	wg.Wait()
	var count, max, distinct, free int
	if err := db.QueryRow(`SELECT count(*), max(deal_number), count(DISTINCT deal_number), count(*) FILTER (WHERE free) FROM deals WHERE status = 'CONFIRMED'`).
		Scan(&count, &max, &distinct, &free); err != nil {
		t.Fatal(err)
	}
	if count != n || max != n || distinct != n || free != 5 {
		t.Fatalf("numbers 1..%d without gaps, first 5 free: count=%d max=%d distinct=%d free=%d", n, count, max, distinct, free)
	}
	if _, err := db.Exec(`UPDATE deals SET deal_number = NULL WHERE id = $1`, ids[0]); err == nil {
		t.Fatalf("a CONFIRMED deal without a number must be refused by the table")
	}
}

// The vendor's answer arrives through the real vendor runtime: "yes"
// answers the deal question; a reply with a price is never swallowed.
func TestIntegration_DealVendorAnswersThroughTheVendorRuntime(t *testing.T) {
	e := newDealEnv(t, "2348070000055", 100)
	ctx := context.Background()
	db := e.db
	router := routing.NewService(NewVendorRepository(db), NewSupplyRepository(db), NewRoutingRepository(db), NewRequestRepository(db),
		testExtractor{}, NewEventWriter(db), NewAuditWriter(db), e.msgr, discard())
	router.SetAutonomy(&routing.VendorAutonomy{Flags: autonomy.AllOn(), Params: autonomy.DefaultParams(), Store: NewAutonomyRepository(db),
		Repo: NewRoutingRepository(db), Quoted: NewConversationRepository(db)})
	router.SetDeals(e.svc)
	if _, err := e.txSvc.Select(ctx, e.s.req, e.offerFor(t, 15000), e.s.buyer, transaction.SourceWhatsApp); err != nil {
		t.Fatal(err)
	}
	d := e.deal(t)
	if err := e.svc.FollowUp(ctx, d.ID, "BOTH"); err != nil {
		t.Fatal(err)
	}
	v, _ := NewVendorRepository(db).FindByID(ctx, e.s.vendor)

	// A reply with a price is a reply of its own, not an answer: the deal
	// question stays open.
	router.ProcessVendorMessageFrom(ctx, v, supply.Inbound{}, "I get another one, 14k")
	if n := e.count(t, `SELECT count(*) FROM clarifications WHERE vendor_id = $1 AND kind = 'vendor_deal_confirm' AND status = 'OPEN'`, v.ID); n != 1 {
		t.Fatalf("not swallowed: the question stays open, got %d", n)
	}
	router.ProcessVendorMessageFrom(ctx, v, supply.Inbound{}, "Yes o")
	if got := e.deal(t); got.VendorAnswer == nil || *got.VendorAnswer != deal.Yes {
		t.Fatalf("vendor answer recorded: %+v", got.VendorAnswer)
	}
	if n := e.count(t, `SELECT count(*) FROM clarifications WHERE vendor_id = $1 AND kind = 'vendor_deal_confirm' AND status = 'ANSWERED'`, v.ID); n != 1 {
		t.Fatalf("answered, got %d", n)
	}
	if n := len(e.msgr.byKind(deal.KindVendorAck)); n != 1 {
		t.Fatalf("vendor thanked once, got %d", n)
	}
}
