package marketplace

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/offer"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/transaction"
)

// ---------------------------------------------------------------------------
// In-memory world. Each fake mirrors the contract of its Postgres
// counterpart (uniqueness, claim semantics), so the marketplace logic is
// exercised against the same rules the database enforces. The DB-level
// guarantees themselves are covered by the Postgres integration tests.
// ---------------------------------------------------------------------------

type world struct {
	mu        sync.Mutex
	requests  map[uuid.UUID]*request.BuyerRequest
	convs     map[uuid.UUID]*conversation.Conversation
	offers    map[uuid.UUID]*offer.Offer
	vrs       map[uuid.UUID]*routing.VendorRequest
	responses map[uuid.UUID]*routing.VendorResponse
	sels      map[uuid.UUID]*transaction.Selection
	txs       map[uuid.UUID]*transaction.Transaction
}

func newWorld() *world {
	return &world{
		requests: map[uuid.UUID]*request.BuyerRequest{}, convs: map[uuid.UUID]*conversation.Conversation{},
		offers: map[uuid.UUID]*offer.Offer{}, vrs: map[uuid.UUID]*routing.VendorRequest{},
		responses: map[uuid.UUID]*routing.VendorResponse{}, sels: map[uuid.UUID]*transaction.Selection{},
		txs: map[uuid.UUID]*transaction.Transaction{},
	}
}

// --- request.Repository ---
type reqRepo struct{ w *world }

func (r reqRepo) Create(ctx context.Context, br *request.BuyerRequest) (*request.BuyerRequest, error) {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	if br.ID == uuid.Nil {
		br.ID = uuid.New()
	}
	br.CreatedAt = time.Now()
	cp := *br
	r.w.requests[br.ID] = &cp
	return &cp, nil
}
func (r reqRepo) FindByID(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error) {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	br, ok := r.w.requests[id]
	if !ok {
		return nil, nil
	}
	cp := *br
	return &cp, nil
}
func (r reqRepo) List(ctx context.Context, s *request.Status) ([]*request.BuyerRequest, error) {
	return nil, nil
}
func (r reqRepo) UpdateExtraction(ctx context.Context, id uuid.UUID, f request.Fields, d map[string]any, c float64, st request.Status) (*request.BuyerRequest, error) {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	br := r.w.requests[id]
	br.Product, br.Status = f.Product, st
	cp := *br
	return &cp, nil
}
func (r reqRepo) UpdateFields(ctx context.Context, id uuid.UUID, f request.Fields) (*request.BuyerRequest, error) {
	return r.FindByID(ctx, id)
}
func (r reqRepo) Confirm(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error) {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	r.w.requests[id].Status = request.StatusConfirmed
	cp := *r.w.requests[id]
	return &cp, nil
}
func (r reqRepo) FindLatestOpenByConversation(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error) {
	return nil, nil
}

// --- conversation.Repository ---
type convRepo struct{ w *world }

func (c convRepo) FindOrCreateByExternalID(ctx context.Context, ch, ext string) (*conversation.Conversation, error) {
	return nil, nil
}
func (c convRepo) FindByID(ctx context.Context, id uuid.UUID) (*conversation.Conversation, error) {
	c.w.mu.Lock()
	defer c.w.mu.Unlock()
	return c.w.convs[id], nil
}
func (c convRepo) SaveMessage(ctx context.Context, m *conversation.Message) (*conversation.Message, error) {
	return m, nil
}
func (c convRepo) MessageExists(ctx context.Context, id uuid.UUID, ext string) (bool, error) {
	return false, nil
}
func (c convRepo) LinkBuyer(ctx context.Context, cid, bid uuid.UUID) error { return nil }

// --- offer.Repository + offer.DeliveryRepository ---
type offerRepo struct {
	w             *world
	failClaimOnce bool
}

func (o *offerRepo) Create(ctx context.Context, of *offer.Offer) (*offer.Offer, error) {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	for _, existing := range o.w.offers {
		if of.VendorResponseID != nil && existing.VendorResponseID != nil && *existing.VendorResponseID == *of.VendorResponseID {
			return nil, offer.ErrAlreadyExists
		}
	}
	if of.ID == uuid.Nil {
		of.ID = uuid.New()
	}
	of.CreatedAt = time.Now()
	cp := *of
	o.w.offers[of.ID] = &cp
	out := cp
	return &out, nil
}
func (o *offerRepo) Update(ctx context.Context, id uuid.UUID, f offer.Fields) (*offer.Offer, error) {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	of := o.w.offers[id]
	if f.Status != nil {
		of.Status = *f.Status
	}
	cp := *of
	return &cp, nil
}
func (o *offerRepo) FindByID(ctx context.Context, id uuid.UUID) (*offer.Offer, error) {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	of, ok := o.w.offers[id]
	if !ok {
		return nil, nil
	}
	cp := *of
	return &cp, nil
}
func (o *offerRepo) ListByBuyerRequest(ctx context.Context, id uuid.UUID) ([]*offer.Offer, error) {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	var out []*offer.Offer
	for _, of := range o.w.offers {
		if of.BuyerRequestID == id {
			cp := *of
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].RankingScore > out[j].RankingScore })
	return out, nil
}
func (o *offerRepo) FindByVendorResponseID(ctx context.Context, id uuid.UUID) (*offer.Offer, error) {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	for _, of := range o.w.offers {
		if of.VendorResponseID != nil && *of.VendorResponseID == id {
			cp := *of
			return &cp, nil
		}
	}
	return nil, nil
}
func (o *offerRepo) ClaimForDelivery(ctx context.Context, id uuid.UUID, stale time.Duration) ([]*offer.Offer, int, error) {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	maxOrd := 0
	var cands []*offer.Offer
	for _, of := range o.w.offers {
		if of.BuyerRequestID != id {
			continue
		}
		if of.PresentationOrdinal != nil && *of.PresentationOrdinal > maxOrd {
			maxOrd = *of.PresentationOrdinal
		}
		if of.Status == offer.StatusReviewed {
			cands = append(cands, of)
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].RankingScore > cands[j].RankingScore })
	var claimed []*offer.Offer
	for _, of := range cands {
		maxOrd++
		n := maxOrd
		of.PresentationOrdinal, of.Status = &n, offer.StatusSending
		of.DeliveryAttempts++
		cp := *of
		claimed = append(claimed, &cp)
	}
	return claimed, maxOrd, nil
}
func (o *offerRepo) MarkDelivered(ctx context.Context, ids []uuid.UUID, sentAt, exp time.Time) error {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	for _, id := range ids {
		of := o.w.offers[id]
		if of.Status == offer.StatusSending {
			s, e := sentAt, exp
			of.Status, of.SentAt, of.ExpiresAt = offer.StatusSent, &s, &e
		}
	}
	return nil
}
func (o *offerRepo) MarkDeliveryFailed(ctx context.Context, ids []uuid.UUID, msg string) error {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	for _, id := range ids {
		of := o.w.offers[id]
		if of.Status == offer.StatusSending {
			m := msg
			of.Status, of.PresentationOrdinal, of.LastDeliveryError = offer.StatusReviewed, nil, &m
		}
	}
	return nil
}
func (o *offerRepo) FindLatestPresentedRequest(ctx context.Context, convID uuid.UUID) (*uuid.UUID, error) {
	o.w.mu.Lock()
	defer o.w.mu.Unlock()
	var best *offer.Offer
	for _, of := range o.w.offers {
		br := o.w.requests[of.BuyerRequestID]
		if br == nil || br.ConversationID != convID || of.PresentationOrdinal == nil || of.SentAt == nil {
			continue
		}
		if best == nil || of.SentAt.After(*best.SentAt) {
			best = of
		}
	}
	if best == nil {
		return nil, nil
	}
	id := best.BuyerRequestID
	return &id, nil
}

// --- routing.Repository (only what the loop uses is meaningful) ---
type routingRepo struct{ w *world }

func (r routingRepo) CreateVendorRequest(ctx context.Context, vr *routing.VendorRequest) (*routing.VendorRequest, error) {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	if vr.ID == uuid.Nil {
		vr.ID = uuid.New()
	}
	r.w.vrs[vr.ID] = vr
	return vr, nil
}
func (r routingRepo) MarkSent(ctx context.Context, id uuid.UUID) error                 { return nil }
func (r routingRepo) MarkSendFailed(ctx context.Context, id uuid.UUID, e string) error { return nil }
func (r routingRepo) FindVendorRequestByID(ctx context.Context, id uuid.UUID) (*routing.VendorRequest, error) {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	return r.w.vrs[id], nil
}
func (r routingRepo) FindVendorRequestByPublicReference(ctx context.Context, ref string) (*routing.VendorRequest, error) {
	return nil, nil
}
func (r routingRepo) ListVendorRequestsByBuyerRequest(ctx context.Context, id uuid.UUID) ([]*routing.VendorRequest, error) {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	var out []*routing.VendorRequest
	for _, vr := range r.w.vrs {
		if vr.BuyerRequestID == id {
			out = append(out, vr)
		}
	}
	return out, nil
}
func (r routingRepo) ListActiveByVendor(ctx context.Context, id uuid.UUID) ([]*routing.VendorRequest, error) {
	return nil, nil
}
func (r routingRepo) ListByVendor(ctx context.Context, id uuid.UUID) ([]*routing.VendorRequest, error) {
	return nil, nil
}
func (r routingRepo) CreateVendorResponse(ctx context.Context, resp *routing.VendorResponse) (*routing.VendorResponse, error) {
	return resp, nil
}
func (r routingRepo) UpdateVendorResponse(ctx context.Context, id uuid.UUID, f routing.ResponseFields) (*routing.VendorResponse, error) {
	return nil, nil
}
func (r routingRepo) FindResponseByID(ctx context.Context, id uuid.UUID) (*routing.VendorResponse, error) {
	return nil, nil
}
func (r routingRepo) FindResponseByVendorRequest(ctx context.Context, id uuid.UUID) (*routing.VendorResponse, error) {
	r.w.mu.Lock()
	defer r.w.mu.Unlock()
	for _, resp := range r.w.responses {
		if resp.VendorRequestID != nil && *resp.VendorRequestID == id {
			return resp, nil
		}
	}
	return nil, nil
}
func (r routingRepo) ListUnmatched(ctx context.Context) ([]*routing.VendorResponse, error) {
	return nil, nil
}
func (r routingRepo) UpdateAssociation(ctx context.Context, rid, vrid uuid.UUID) (*uuid.UUID, *routing.VendorResponse, error) {
	return nil, nil, nil
}

// --- transaction.Repository ---
type txRepo struct{ w *world }

func (t txRepo) CreateSelectionWithTransaction(ctx context.Context, s *transaction.Selection, tx *transaction.Transaction) (*transaction.Selection, *transaction.Transaction, error) {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	for _, existing := range t.w.sels {
		if existing.BuyerRequestID == s.BuyerRequestID && existing.Status == transaction.SelectionActive {
			return nil, nil, transaction.ErrSelectionExists // the unique index
		}
	}
	s.ID, s.Status, tx.ID = uuid.New(), transaction.SelectionActive, uuid.New()
	tx.BuyerSelectionID = s.ID
	t.w.sels[s.ID], t.w.txs[tx.ID] = s, tx
	t.w.offers[s.OfferID].Status = offer.StatusAccepted
	return s, tx, nil
}
func (t txRepo) FindSelectionByBuyerRequest(ctx context.Context, id uuid.UUID) (*transaction.Selection, error) {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	for _, s := range t.w.sels {
		if s.BuyerRequestID == id && s.Status == transaction.SelectionActive {
			return s, nil
		}
	}
	return nil, nil
}
func (t txRepo) FindTransactionByID(ctx context.Context, id uuid.UUID) (*transaction.Transaction, error) {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	return t.w.txs[id], nil
}
func (t txRepo) FindTransactionBySelection(ctx context.Context, id uuid.UUID) (*transaction.Transaction, error) {
	t.w.mu.Lock()
	defer t.w.mu.Unlock()
	for _, tx := range t.w.txs {
		if tx.BuyerSelectionID == id {
			return tx, nil
		}
	}
	return nil, nil
}

// --- infra fakes ---
type outbound struct {
	mu   sync.Mutex
	sent []sentMsg
	fail error
}
type sentMsg struct{ to, body string }

func (o *outbound) SendText(ctx context.Context, to, body string) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.fail != nil {
		return o.fail
	}
	o.sent = append(o.sent, sentMsg{to, body})
	return nil
}
func (o *outbound) count() int { o.mu.Lock(); defer o.mu.Unlock(); return len(o.sent) }
func (o *outbound) last() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.sent) == 0 {
		return ""
	}
	return o.sent[len(o.sent)-1].body
}

type events struct {
	mu  sync.Mutex
	all []*event.Event
}

func (e *events) Write(ctx context.Context, ev *event.Event) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.all = append(e.all, ev)
	return nil
}
func (e *events) count(t event.Type) int {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	for _, ev := range e.all {
		if ev.EventType == t {
			n++
		}
	}
	return n
}

type audits struct{}

func (audits) Write(ctx context.Context, e *audit.Entry) error { return nil }

type nopAI struct{}

func (nopAI) Extract(ctx context.Context, r ai.ExtractionRequest) (*ai.ExtractionResult, error) {
	p := "ps5"
	return &ai.ExtractionResult{Product: &p, Confidence: 0.8}, nil
}
func (nopAI) ExtractSupply(ctx context.Context, t string) (*ai.SupplyExtractionResult, error) {
	return nil, errors.New("unused")
}
func (nopAI) ExtractVendorResponse(ctx context.Context, t string) (*ai.VendorResponseExtractionResult, error) {
	return nil, errors.New("unused")
}
func (nopAI) Healthy(ctx context.Context) error { return nil }

type syncJobs struct{ ran int }

func (j *syncJobs) Enqueue(job func(context.Context)) { j.ran++; job(context.Background()) }

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func sp(s string) *string       { return &s }
func i64(n int64) *int64        { return &n }

// harness wires the real services over the in-memory world.
type harness struct {
	w        *world
	out      *outbound
	ev       *events
	jobs     *syncJobs
	offers   *offerRepo
	delivery *OfferDelivery
	inbound  *BuyerInbound
	txSvc    *transaction.Service

	convID, buyerID, requestID uuid.UUID
	buyerPhone                 string
}

func newHarness(t *testing.T, status request.Status) *harness {
	t.Helper()
	w := newWorld()
	h := &harness{w: w, out: &outbound{}, ev: &events{}, jobs: &syncJobs{}, offers: &offerRepo{w: w}}
	h.convID, h.buyerID, h.requestID, h.buyerPhone = uuid.New(), uuid.New(), uuid.New(), "2348099999999"
	w.convs[h.convID] = &conversation.Conversation{ID: h.convID, Channel: "whatsapp", ExternalID: h.buyerPhone}
	w.requests[h.requestID] = &request.BuyerRequest{
		ID: h.requestID, ConversationID: h.convID, BuyerID: h.buyerID, Status: status,
		Product: sp("iphone 13"), Variant: sp("128GB"), Condition: sp("used"), BudgetMax: i64(650000),
		Location: sp("Benin City"), Currency: "NGN", CreatedAt: time.Now(),
	}

	rq, rr := reqRepo{w}, routingRepo{w}
	offerSvc := offer.NewService(h.offers, rr, rq, h.ev, audits{}, quietLogger())
	h.txSvc = transaction.NewService(txRepo{w}, h.offers, rq, h.ev, audits{}, quietLogger())
	h.delivery = NewOfferDelivery(rq, convRepo{w}, offerSvc, h.offers, h.offers, rr, txRepo{w}, h.out, h.ev, quietLogger())
	reqSvc := request.NewService(rq, convRepo{w}, nopAI{}, h.ev, audits{}, h.out, quietLogger())
	h.inbound = NewBuyerInbound(NewContextResolver(h.offers, h.offers), reqSvc, rq, h.txSvc, h.offers, rr, h.out, h.ev, h.jobs, quietLogger())
	return h
}

// vendorReplies adds a VendorRequest (with MM reference) + an eligible
// VendorResponse for the harness request and returns the response.
func (h *harness) vendorReplies(ref string, status routing.ResponseStatus, review routing.ReviewState, variant *string, price int64) *routing.VendorResponse {
	vr := &routing.VendorRequest{ID: uuid.New(), BuyerRequestID: h.requestID, VendorID: uuid.New(), PublicReference: ref, Status: routing.VendorRequestResponded}
	h.w.vrs[vr.ID] = vr
	resp := &routing.VendorResponse{
		ID: uuid.New(), VendorRequestID: &vr.ID, VendorID: vr.VendorID, Status: status, ReviewState: review,
		Variant: variant, Price: i64(price), Confidence: 0.9,
	}
	h.w.responses[resp.ID] = resp
	return resp
}

func (h *harness) buyerSays(text string) {
	h.inbound.Handle(context.Background(), InboundBuyerMessage{
		ConversationID: h.convID, BuyerID: h.buyerID, MessageID: uuid.New(), ReplyTo: h.buyerPhone, Type: "text", Text: text,
	})
}

func (h *harness) countRequests() int { h.w.mu.Lock(); defer h.w.mu.Unlock(); return len(h.w.requests) }
func (h *harness) countSelections() int {
	h.w.mu.Lock()
	defer h.w.mu.Unlock()
	return len(h.w.sels)
}
func (h *harness) countTxs() int { h.w.mu.Lock(); defer h.w.mu.Unlock(); return len(h.w.txs) }

// ---------------------------------------------------------------------------
// Offer delivery
// ---------------------------------------------------------------------------

func TestDelivery_EligibleVendorResponseIsDeliveredToBuyer(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	resp := h.vendorReplies("MM-382910", routing.ResponseAvailable, routing.ReviewReviewed, nil, 620000)

	h.delivery.VendorResponseChanged(context.Background(), resp) // what routing calls after persisting the reply

	if h.out.count() != 1 {
		t.Fatalf("expected exactly 1 WhatsApp message to the buyer, got %d", h.out.count())
	}
	if h.out.sent[0].to != h.buyerPhone {
		t.Fatalf("offer sent to wrong recipient %q", h.out.sent[0].to)
	}
	msg := h.out.last()
	for _, want := range []string{"1. iphone 13 128GB", "₦620,000", "Available now", "used", "Reply 1"} {
		if !strings.Contains(msg, want) {
			t.Errorf("buyer message missing %q:\n%s", want, msg)
		}
	}
	offers, _ := h.offers.ListByBuyerRequest(context.Background(), h.requestID)
	if len(offers) != 1 || offers[0].Status != offer.StatusSent || *offers[0].PresentationOrdinal != 1 || offers[0].SentAt == nil || offers[0].ExpiresAt == nil {
		t.Fatalf("expected one SENT offer with ordinal 1, sent_at and expires_at, got %+v", offers[0])
	}
	if h.ev.count(event.OfferCreated) != 1 || h.ev.count(event.OfferSent) != 1 {
		t.Fatalf("expected OFFER_CREATED and OFFER_SENT once each, got created=%d sent=%d", h.ev.count(event.OfferCreated), h.ev.count(event.OfferSent))
	}
}

func TestDelivery_AlreadyDeliveredOfferIsNotSentAgain(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	resp := h.vendorReplies("MM-382910", routing.ResponseAvailable, routing.ReviewReviewed, nil, 620000)

	h.delivery.VendorResponseChanged(context.Background(), resp)
	// Same response re-notified (e.g. operator re-saves it), an explicit
	// operator deliver, and a burst of concurrent triggers:
	h.delivery.VendorResponseChanged(context.Background(), resp)
	if _, err := h.delivery.DeliverPendingOffers(context.Background(), h.requestID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); h.delivery.VendorResponseChanged(context.Background(), resp) }()
	}
	wg.Wait()

	if h.out.count() != 1 {
		t.Fatalf("same VendorResponse -> one logical Offer -> one delivery; got %d messages", h.out.count())
	}
	offers, _ := h.offers.ListByBuyerRequest(context.Background(), h.requestID)
	if len(offers) != 1 {
		t.Fatalf("expected 1 offer, got %d", len(offers))
	}
}

func TestDelivery_SecondVendorOfferGetsNextOrdinalAndOnlyNewOptionIsSent(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	first := h.vendorReplies("MM-382910", routing.ResponseAvailable, routing.ReviewReviewed, nil, 620000)
	h.delivery.VendorResponseChanged(context.Background(), first)
	second := h.vendorReplies("MM-518204", routing.ResponseAlternative, routing.ReviewReviewed, sp("256GB"), 700000)
	h.delivery.VendorResponseChanged(context.Background(), second)

	if h.out.count() != 2 {
		t.Fatalf("expected 2 buyer messages, got %d", h.out.count())
	}
	msg := h.out.last()
	if strings.Contains(msg, "₦620,000") {
		t.Errorf("follow-up must not resend option 1:\n%s", msg)
	}
	for _, want := range []string{"another option", "2. iphone 13 256GB", "₦700,000", "Alternative", "(1-2)"} {
		if !strings.Contains(msg, want) {
			t.Errorf("follow-up missing %q:\n%s", want, msg)
		}
	}
}

func TestDelivery_FailureIsRecordedAndOfferStaysDeliverable(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	resp := h.vendorReplies("MM-382910", routing.ResponseAvailable, routing.ReviewReviewed, nil, 620000)
	h.out.fail = errors.New(`whatsapp api returned status 400: {"error":{"code":131047}}`)

	_, err := h.delivery.DeliverPendingOffers(context.Background(), h.requestID)
	if !errors.Is(err, ErrDeliveryFailed) {
		t.Fatalf("expected ErrDeliveryFailed, got %v", err)
	}
	offers, _ := h.offers.ListByBuyerRequest(context.Background(), h.requestID)
	o := offers[0]
	if o.Status != offer.StatusReviewed || o.PresentationOrdinal != nil || o.LastDeliveryError == nil || !strings.Contains(*o.LastDeliveryError, "131047") {
		t.Fatalf("failed delivery must leave the offer REVIEWED, unnumbered, with the error recorded; got %+v", o)
	}
	if h.ev.count(event.OfferSendFailed) != 1 || h.ev.count(event.OfferSent) != 0 {
		t.Fatalf("expected OFFER_SEND_FAILED=1 OFFER_SENT=0, got %d/%d", h.ev.count(event.OfferSendFailed), h.ev.count(event.OfferSent))
	}

	// Retry succeeds and the offer gets ordinal 1.
	h.out.fail = nil
	h.delivery.VendorResponseChanged(context.Background(), resp)
	offers, _ = h.offers.ListByBuyerRequest(context.Background(), h.requestID)
	if offers[0].Status != offer.StatusSent || *offers[0].PresentationOrdinal != 1 || h.out.count() != 1 {
		t.Fatalf("retry should deliver as option 1, got %+v (messages=%d)", offers[0], h.out.count())
	}
}

func TestDelivery_InternalMetadataIsNeverExposed(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	resp := h.vendorReplies("MM-382910", routing.ResponseAvailable, routing.ReviewReviewed, nil, 620000)
	h.delivery.VendorResponseChanged(context.Background(), resp)

	offers, _ := h.offers.ListByBuyerRequest(context.Background(), h.requestID)
	o := offers[0]
	msg := h.out.last()
	forbidden := []string{
		fmt.Sprintf("%d", o.RankingScore), "score", "Score", "confidence", "0.9", "+30", "+20", "within budget",
		o.ID.String(), o.VendorID.String(), h.requestID.String(), "MM-382910", "REVIEWED", "EXACT", "vendor_",
	}
	for _, f := range forbidden {
		if f != "" && strings.Contains(msg, f) {
			t.Errorf("buyer-facing offer message leaks %q:\n%s", f, msg)
		}
	}
}

func TestDelivery_UnconfirmedRequestNeverGetsOffers(t *testing.T) {
	h := newHarness(t, request.StatusNeedsReview)
	resp := h.vendorReplies("MM-382910", routing.ResponseAvailable, routing.ReviewReviewed, nil, 620000)

	h.delivery.VendorResponseChanged(context.Background(), resp)

	if h.out.count() != 0 {
		t.Fatalf("unconfirmed request must not receive offers, got %d messages", h.out.count())
	}
	if offers, _ := h.offers.ListByBuyerRequest(context.Background(), h.requestID); len(offers) != 0 {
		t.Fatalf("unconfirmed request must not get offers built, got %d", len(offers))
	}
	if _, err := h.delivery.DeliverPendingOffers(context.Background(), h.requestID); !errors.Is(err, request.ErrNotConfirmed) {
		t.Fatalf("expected ErrNotConfirmed, got %v", err)
	}
}

func TestDelivery_NeedsReviewResponseIsNotDelivered(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	resp := h.vendorReplies("MM-382910", routing.ResponseUnclassified, routing.ReviewNeedsReview, nil, 620000)
	h.delivery.VendorResponseChanged(context.Background(), resp)
	if h.out.count() != 0 {
		t.Fatalf("an UNCLASSIFIED/NEEDS_REVIEW reply must never reach the buyer, got %d messages", h.out.count())
	}
}

func TestDelivery_ResponseCorrectedToUnavailableWithdrawsOffer(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	resp := h.vendorReplies("MM-382910", routing.ResponseAvailable, routing.ReviewReviewed, nil, 620000)
	h.delivery.VendorResponseChanged(context.Background(), resp)

	corrected := *resp
	corrected.Status, corrected.ReviewState = routing.ResponseUnavailable, routing.ReviewCorrected
	h.delivery.VendorResponseChanged(context.Background(), &corrected)

	offers, _ := h.offers.ListByBuyerRequest(context.Background(), h.requestID)
	if offers[0].Status != offer.StatusWithdrawn || h.ev.count(event.OfferWithdrawn) != 1 {
		t.Fatalf("expected offer WITHDRAWN + event, got %s", offers[0].Status)
	}
	h.buyerSays("1")
	if h.countSelections() != 0 || !strings.Contains(h.out.last(), "no longer available") {
		t.Fatalf("withdrawn offer must not be selectable; reply was:\n%s", h.out.last())
	}
}

// ---------------------------------------------------------------------------
// Context resolution
// ---------------------------------------------------------------------------

func TestParseSelectionReply(t *testing.T) {
	numbers := map[string]int{
		"1": 1, " 2 ": 2, "#3": 3, "option 1": 1, "Option 2.": 2, "number 3": 3, "no. 2": 2, "no 2": 2,
		"I'll take 1": 1, "i will take 2": 2, "I want 1": 1, "I want option 2": 2, "give me 3 please": 3,
		"I go take 1": 1, "make I take 2": 2, "2 abeg": 2, "pick 1!": 1, "I’ll take 2": 2, "0": 0, "12": 12,
	}
	for in, want := range numbers {
		got := ParseSelectionReply(in)
		if got.Kind != ReplyNumber || got.Ordinal != want {
			t.Errorf("ParseSelectionReply(%q) = %+v, want number %d", in, got, want)
		}
	}
	for _, in := range []string{"yes", "Yes!", "ok", "this one", "I'll take it", "go for am", "buy it"} {
		if got := ParseSelectionReply(in); got.Kind != ReplyAffirmative {
			t.Errorf("ParseSelectionReply(%q) = %+v, want affirmative", in, got)
		}
	}
	for _, in := range []string{
		"I need an iPhone 13 128GB, budget 650k.", "I need 2 iphones", "2 bags of rice", "100k", "650k",
		"PS5 slim in Lagos", "how much is delivery", "",
	} {
		if got := ParseSelectionReply(in); got.Kind != ReplyNone {
			t.Errorf("ParseSelectionReply(%q) = %+v, want none (new request)", in, got)
		}
	}
}

func TestResolve_NoPendingContextCreatesNewBuyerRequest(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	before := h.countRequests()
	h.buyerSays("I need a PS5 slim in Lagos, 500k")
	if h.countRequests() != before+1 || h.jobs.ran != 1 {
		t.Fatalf("expected a new BuyerRequest with extraction queued; requests %d->%d, jobs=%d", before, h.countRequests(), h.jobs.ran)
	}
	if h.out.last() != msgAck {
		t.Fatalf("expected the Phase 1 acknowledgement, got %q", h.out.last())
	}
}

func TestResolve_BareNumberWithoutContextIsNotANewRequest(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	before := h.countRequests()
	h.buyerSays("1")
	if h.countRequests() != before {
		t.Fatal(`"1" with no open options must not create a BuyerRequest (pre-3.1 bug: raw_message="1")`)
	}
	if h.out.last() != msgNoOpenOptions || h.ev.count(event.BuyerReplyUnresolved) != 1 {
		t.Fatalf("expected no-open-options reply + BUYER_REPLY_UNRESOLVED, got %q", h.out.last())
	}
}

func twoOfferHarness(t *testing.T) (*harness, []*offer.Offer) {
	h := newHarness(t, request.StatusConfirmed)
	a := h.vendorReplies("MM-382910", routing.ResponseAvailable, routing.ReviewReviewed, nil, 620000)
	b := h.vendorReplies("MM-518204", routing.ResponseAlternative, routing.ReviewReviewed, sp("256GB"), 700000)
	h.w.responses[a.ID], h.w.responses[b.ID] = a, b
	if _, err := h.delivery.DeliverPendingOffers(context.Background(), h.requestID); err != nil {
		t.Fatal(err)
	}
	offers, _ := h.offers.ListByBuyerRequest(context.Background(), h.requestID)
	sort.Slice(offers, func(i, j int) bool { return *offers[i].PresentationOrdinal < *offers[j].PresentationOrdinal })
	return h, offers
}

func TestResolve_AwaitingSelectionReplyOneSelectsOfferOne(t *testing.T) {
	h, offers := twoOfferHarness(t)
	res, err := NewContextResolver(h.offers, h.offers).Resolve(context.Background(), h.convID, "1")
	if err != nil || res.Kind != ResolveSelectOffer || res.OfferID != offers[0].ID || res.BuyerRequestID != h.requestID {
		t.Fatalf(`"1" should resolve to offer #1 (%s), got %+v err=%v`, offers[0].ID, res, err)
	}
}

func TestResolve_AwaitingSelectionReplyTwoSelectsOfferTwo(t *testing.T) {
	h, offers := twoOfferHarness(t)
	res, _ := NewContextResolver(h.offers, h.offers).Resolve(context.Background(), h.convID, "I'll take 2")
	if res.Kind != ResolveSelectOffer || res.OfferID != offers[1].ID {
		t.Fatalf(`"I'll take 2" should resolve to offer #2 (%s), got %+v`, offers[1].ID, res)
	}
}

func TestResolve_InvalidNumberAsksForClarificationWithoutSelecting(t *testing.T) {
	h, _ := twoOfferHarness(t)
	h.buyerSays("5")
	if h.countSelections() != 0 || h.countTxs() != 0 {
		t.Fatal("out-of-range number must not select anything")
	}
	if !strings.Contains(h.out.last(), "I don't have an option 5") || !strings.Contains(h.out.last(), "(1-2)") {
		t.Fatalf("expected out-of-range clarification, got %q", h.out.last())
	}
}

func TestResolve_AmbiguousYesChangesNoState(t *testing.T) {
	h, _ := twoOfferHarness(t)
	reqs := h.countRequests()
	h.buyerSays("yes")
	if h.countSelections() != 0 || h.countTxs() != 0 || h.countRequests() != reqs {
		t.Fatal(`"yes" must not select an offer or create a request`)
	}
	if !strings.Contains(h.out.last(), "Just to be sure") {
		t.Fatalf("expected a clarification, got %q", h.out.last())
	}
	if h.ev.count(event.BuyerReplyUnresolved) != 1 {
		t.Fatal("expected BUYER_REPLY_UNRESOLVED event")
	}
}

func TestResolve_AmbiguousYesWithSingleOfferStillDoesNotSelect(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	resp := h.vendorReplies("MM-382910", routing.ResponseAvailable, routing.ReviewReviewed, nil, 620000)
	h.delivery.VendorResponseChanged(context.Background(), resp)
	h.buyerSays("yes")
	if h.countSelections() != 0 {
		t.Fatal(`ambiguous "yes" must never trigger a selection, even with one option`)
	}
	if !strings.Contains(h.out.last(), "Reply 1") {
		t.Fatalf("expected 'Reply 1' hint, got %q", h.out.last())
	}
}

func TestResolve_UnrelatedNewRequestStillCreatedWhileOffersPending(t *testing.T) {
	h, _ := twoOfferHarness(t)
	before := h.countRequests()
	h.buyerSays("Abeg I need PS5 slim for Lagos, 550k")
	if h.countRequests() != before+1 {
		t.Fatal("a clearly new request must still create a BuyerRequest while an offer context exists")
	}
	if h.countSelections() != 0 {
		t.Fatal("a new request must not select anything")
	}
}

// ---------------------------------------------------------------------------
// Selection through WhatsApp
// ---------------------------------------------------------------------------

func TestSelection_ReplyCreatesExactlyOneSelectionAndTransaction(t *testing.T) {
	h, offers := twoOfferHarness(t)
	h.buyerSays("1")

	if h.countSelections() != 1 || h.countTxs() != 1 {
		t.Fatalf("expected exactly 1 selection and 1 transaction, got %d/%d", h.countSelections(), h.countTxs())
	}
	for _, tx := range h.w.txs {
		if tx.PaymentStatus != transaction.PaymentUnpaid || tx.OfferID != offers[0].ID || tx.GrossAmount != 620000 {
			t.Fatalf("unexpected transaction %+v", tx)
		}
	}
	o, _ := h.offers.FindByID(context.Background(), offers[0].ID)
	if o.Status != offer.StatusAccepted {
		t.Fatalf("selected offer should be ACCEPTED, got %s", o.Status)
	}
	msg := h.out.last()
	for _, want := range []string{"recorded your choice", "₦620,000", "Reference: MM-", "Nothing has been paid"} {
		if !strings.Contains(msg, want) {
			t.Errorf("confirmation missing %q:\n%s", want, msg)
		}
	}
	for _, bad := range []string{"paid ✅", "payment received", "delivered", "on its way"} {
		if strings.Contains(strings.ToLower(msg), strings.ToLower(bad)) {
			t.Errorf("confirmation must not claim %q:\n%s", bad, msg)
		}
	}
}

func TestSelection_RepeatedSameReplyDoesNotDuplicate(t *testing.T) {
	h, _ := twoOfferHarness(t)
	h.buyerSays("1")
	h.buyerSays("1") // buyer sends "1" again (new WhatsApp message ID)
	h.buyerSays("option 1")

	if h.countSelections() != 1 || h.countTxs() != 1 {
		t.Fatalf("repeated replies must not duplicate; got %d selections, %d transactions", h.countSelections(), h.countTxs())
	}
	if !strings.Contains(h.out.last(), "already chosen option 1") {
		t.Fatalf("expected idempotent re-confirmation, got %q", h.out.last())
	}
}

func TestSelection_DifferentOptionAfterSelectionIsRefused(t *testing.T) {
	h, _ := twoOfferHarness(t)
	h.buyerSays("1")
	h.buyerSays("2")
	if h.countSelections() != 1 || h.countTxs() != 1 {
		t.Fatal("one BuyerRequest -> at most one selection")
	}
	if !strings.Contains(h.out.last(), "already chosen option 1") {
		t.Fatalf("expected the held option to be named, got %q", h.out.last())
	}
}

func TestSelection_ConcurrentRepliesCreateOneTransaction(t *testing.T) {
	h, _ := twoOfferHarness(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h.buyerSays([]string{"1", "2"}[i%2])
		}(i)
	}
	wg.Wait()
	if h.countSelections() != 1 || h.countTxs() != 1 {
		t.Fatalf("concurrent replies must yield exactly 1 selection/transaction, got %d/%d", h.countSelections(), h.countTxs())
	}
}

func TestSelection_ExpiredOfferIsNotSelectable(t *testing.T) {
	h, offers := twoOfferHarness(t)
	past := time.Now().Add(-time.Minute)
	h.w.offers[offers[0].ID].ExpiresAt = &past
	h.buyerSays("1")
	if h.countTxs() != 0 || !strings.Contains(h.out.last(), "no longer available") {
		t.Fatalf("expired offer must not be selected; reply %q", h.out.last())
	}
}

func TestSelection_NoNewOffersDeliveredAfterSelection(t *testing.T) {
	h, _ := twoOfferHarness(t)
	h.buyerSays("1")
	sent := h.out.count()
	late := h.vendorReplies("MM-777777", routing.ResponseAvailable, routing.ReviewReviewed, nil, 600000)
	h.delivery.VendorResponseChanged(context.Background(), late)
	if h.out.count() != sent {
		t.Fatal("a decided request must not receive further offers")
	}
}

func TestInbound_MediaMessageGetsGracefulReplyAndCreatesNothing(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	before := h.countRequests()
	h.inbound.Handle(context.Background(), InboundBuyerMessage{
		ConversationID: h.convID, BuyerID: h.buyerID, MessageID: uuid.New(), ReplyTo: h.buyerPhone, Type: "audio",
		Media: &conversation.MediaRef{Kind: "audio", Provider: "whatsapp", ProviderRef: "MEDIA123", MimeType: "audio/ogg; codecs=opus", Voice: true},
	})
	if h.countRequests() != before || h.out.last() != msgUnsupportedMedia {
		t.Fatalf("voice note: expected graceful reply and no request, got %q", h.out.last())
	}
}

// ---------------------------------------------------------------------------
// §23A funnel data: the events of the core loop carry the dimensions needed
// to measure it later (who, which request/offer/vendor, what, where, how).
// ---------------------------------------------------------------------------

func eventsOf(h *harness, t event.Type) []*event.Event {
	h.ev.mu.Lock()
	defer h.ev.mu.Unlock()
	var out []*event.Event
	for _, e := range h.ev.all {
		if e.EventType == t {
			out = append(out, e)
		}
	}
	return out
}

func TestFunnel_CoreLoopEventsCarryMeasurementDimensions(t *testing.T) {
	h, offers := twoOfferHarness(t)
	h.buyerSays("1")

	required := map[event.Type][]string{
		event.OfferCreated:          {"buyer_request_id", "buyer_id", "offer_id", "vendor_id", "vendor_request_id", "product", "location", "offer_type", "price"},
		event.OfferSent:             {"buyer_request_id", "buyer_id", "offer_id", "vendor_id", "product", "location", "channel", "ordinal", "outcome"},
		event.BuyerSelectionCreated: {"buyer_request_id", "buyer_id", "offer_id", "vendor_id", "product", "location", "price", "source", "transaction_id"},
		event.OfferAccepted:         {"buyer_request_id", "offer_id", "vendor_id", "source"},
		event.TransactionCreated:    {"buyer_request_id", "buyer_id", "offer_id", "vendor_id", "gross_amount", "payment_status", "source", "location"},
	}
	for typ, keys := range required {
		evs := eventsOf(h, typ)
		if len(evs) == 0 {
			t.Fatalf("no %s event written", typ)
		}
		for _, k := range keys {
			if _, ok := evs[0].Payload[k]; !ok {
				t.Errorf("%s payload missing %q: %v", typ, k, evs[0].Payload)
			}
		}
	}
	sel := eventsOf(h, event.BuyerSelectionCreated)[0]
	if sel.Payload["source"] != transaction.SourceWhatsApp || sel.Payload["offer_id"] != offers[0].ID.String() {
		t.Fatalf("selection must record source=whatsapp_reply and the chosen offer, got %v", sel.Payload)
	}
	if sel.Payload["buyer_request_id"] != h.requestID.String() || sel.Payload["location"] != "Benin City" {
		t.Fatalf("selection must carry the buyer/request relationship and known location, got %v", sel.Payload)
	}
}

func TestFunnel_UnknownDimensionsAreOmittedNotGuessed(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	h.w.requests[h.requestID].Location = nil // buyer never said where
	h.w.requests[h.requestID].Category = nil
	resp := h.vendorReplies("MM-382910", routing.ResponseAvailable, routing.ReviewReviewed, nil, 620000)
	h.delivery.VendorResponseChanged(context.Background(), resp)
	for _, e := range eventsOf(h, event.OfferSent) {
		if _, ok := e.Payload["location"]; ok {
			t.Fatalf("location must be omitted when unknown, got %v", e.Payload["location"])
		}
		if _, ok := e.Payload["category"]; ok {
			t.Fatalf("category must be omitted when unknown, got %v", e.Payload["category"])
		}
	}
}

func (r reqRepo) FindDiscoveryAsk(ctx context.Context, deliveryID, listingID uuid.UUID) (*request.BuyerRequest, error) {
	return nil, nil
}

func (r reqRepo) SetStatus(ctx context.Context, id uuid.UUID, from []request.Status, to request.Status) (*request.BuyerRequest, error) {
	return nil, nil
}
