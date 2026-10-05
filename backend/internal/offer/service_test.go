package offer

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
)

// --- fakes ---

type fakeOfferRepo struct {
	offers map[uuid.UUID]*Offer
}

func newFakeOfferRepo() *fakeOfferRepo { return &fakeOfferRepo{offers: map[uuid.UUID]*Offer{}} }

func (f *fakeOfferRepo) Create(ctx context.Context, o *Offer) (*Offer, error) {
	if o.ID == uuid.Nil {
		o.ID = uuid.New()
	}
	cp := *o
	f.offers[o.ID] = &cp
	return &cp, nil
}
func (f *fakeOfferRepo) Update(ctx context.Context, id uuid.UUID, fields Fields) (*Offer, error) {
	o, ok := f.offers[id]
	if !ok {
		return nil, nil
	}
	if fields.Status != nil {
		o.Status = *fields.Status
	}
	cp := *o
	return &cp, nil
}
func (f *fakeOfferRepo) FindByID(ctx context.Context, id uuid.UUID) (*Offer, error) {
	o, ok := f.offers[id]
	if !ok {
		return nil, nil
	}
	cp := *o
	return &cp, nil
}
func (f *fakeOfferRepo) ListByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*Offer, error) {
	var out []*Offer
	for _, o := range f.offers {
		if o.BuyerRequestID == buyerRequestID {
			cp := *o
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (f *fakeOfferRepo) FindByVendorResponseID(ctx context.Context, vendorResponseID uuid.UUID) (*Offer, error) {
	for _, o := range f.offers {
		if o.VendorResponseID != nil && *o.VendorResponseID == vendorResponseID {
			cp := *o
			return &cp, nil
		}
	}
	return nil, nil
}

type fakeRoutingRepoForOffers struct {
	vendorRequests map[uuid.UUID]*routing.VendorRequest
	responses      map[uuid.UUID]*routing.VendorResponse // keyed by vendor_request_id
}

func (f *fakeRoutingRepoForOffers) CreateVendorRequest(ctx context.Context, vr *routing.VendorRequest) (*routing.VendorRequest, error) {
	return nil, nil
}
func (f *fakeRoutingRepoForOffers) MarkSent(ctx context.Context, id uuid.UUID) error { return nil }
func (f *fakeRoutingRepoForOffers) MarkSendFailed(ctx context.Context, id uuid.UUID, sendErr string) error {
	return nil
}
func (f *fakeRoutingRepoForOffers) FindVendorRequestByID(ctx context.Context, id uuid.UUID) (*routing.VendorRequest, error) {
	return f.vendorRequests[id], nil
}
func (f *fakeRoutingRepoForOffers) FindVendorRequestByPublicReference(ctx context.Context, ref string) (*routing.VendorRequest, error) {
	return nil, nil
}
func (f *fakeRoutingRepoForOffers) ListVendorRequestsByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*routing.VendorRequest, error) {
	var out []*routing.VendorRequest
	for _, vr := range f.vendorRequests {
		if vr.BuyerRequestID == buyerRequestID {
			out = append(out, vr)
		}
	}
	return out, nil
}
func (f *fakeRoutingRepoForOffers) ListActiveByVendor(ctx context.Context, vendorID uuid.UUID) ([]*routing.VendorRequest, error) {
	return nil, nil
}
func (f *fakeRoutingRepoForOffers) ListByVendor(ctx context.Context, vendorID uuid.UUID) ([]*routing.VendorRequest, error) {
	return nil, nil
}
func (f *fakeRoutingRepoForOffers) CreateVendorResponse(ctx context.Context, resp *routing.VendorResponse) (*routing.VendorResponse, error) {
	return nil, nil
}
func (f *fakeRoutingRepoForOffers) UpdateVendorResponse(ctx context.Context, id uuid.UUID, fields routing.ResponseFields) (*routing.VendorResponse, error) {
	return nil, nil
}
func (f *fakeRoutingRepoForOffers) FindResponseByID(ctx context.Context, id uuid.UUID) (*routing.VendorResponse, error) {
	return nil, nil
}
func (f *fakeRoutingRepoForOffers) FindResponseByVendorRequest(ctx context.Context, vendorRequestID uuid.UUID) (*routing.VendorResponse, error) {
	return f.responses[vendorRequestID], nil
}
func (f *fakeRoutingRepoForOffers) ListUnmatched(ctx context.Context) ([]*routing.VendorResponse, error) {
	return nil, nil
}
func (f *fakeRoutingRepoForOffers) UpdateAssociation(ctx context.Context, responseID uuid.UUID, newVendorRequestID uuid.UUID) (*uuid.UUID, *routing.VendorResponse, error) {
	return nil, nil, nil
}

type fakeRequestRepoForOffers struct {
	requests map[uuid.UUID]*request.BuyerRequest
}

func (f *fakeRequestRepoForOffers) Create(ctx context.Context, r *request.BuyerRequest) (*request.BuyerRequest, error) {
	return nil, nil
}
func (f *fakeRequestRepoForOffers) FindByID(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error) {
	return f.requests[id], nil
}
func (f *fakeRequestRepoForOffers) List(ctx context.Context, statusFilter *request.Status) ([]*request.BuyerRequest, error) {
	return nil, nil
}
func (f *fakeRequestRepoForOffers) UpdateExtraction(ctx context.Context, id uuid.UUID, fields request.Fields, extractedData map[string]any, confidence float64, newStatus request.Status) (*request.BuyerRequest, error) {
	return nil, nil
}
func (f *fakeRequestRepoForOffers) UpdateFields(ctx context.Context, id uuid.UUID, fields request.Fields) (*request.BuyerRequest, error) {
	return nil, nil
}
func (f *fakeRequestRepoForOffers) Confirm(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error) {
	return nil, nil
}
func (f *fakeRequestRepoForOffers) FindLatestOpenByConversation(ctx context.Context, conversationID uuid.UUID) (*request.BuyerRequest, error) {
	return nil, nil
}

type fakeEventsForOffers struct{ events []*event.Event }

func (f *fakeEventsForOffers) Write(ctx context.Context, e *event.Event) error {
	f.events = append(f.events, e)
	return nil
}

type fakeAuditsForOffers struct{ entries []*audit.Entry }

func (f *fakeAuditsForOffers) Write(ctx context.Context, e *audit.Entry) error {
	f.entries = append(f.entries, e)
	return nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func strp(s string) *string    { return &s }
func i64p(i int64) *int64      { return &i }

// --- tests ---

func TestBuildForBuyerRequest_EligibleAvailableResponseBecomesExactOffer(t *testing.T) {
	buyerReqID := uuid.New()
	req := &request.BuyerRequest{ID: buyerReqID, Status: request.StatusConfirmed, Product: strp("iPhone 13"), BudgetMax: i64p(700000)}
	vr := &routing.VendorRequest{ID: uuid.New(), BuyerRequestID: buyerReqID, VendorID: uuid.New()}
	resp := &routing.VendorResponse{
		ID: uuid.New(), VendorRequestID: &vr.ID, Status: routing.ResponseAvailable, ReviewState: routing.ReviewReviewed,
		Product: strp("iPhone 13"), Price: i64p(650000), Confidence: 0.9,
	}

	routingRepo := &fakeRoutingRepoForOffers{
		vendorRequests: map[uuid.UUID]*routing.VendorRequest{vr.ID: vr},
		responses:      map[uuid.UUID]*routing.VendorResponse{vr.ID: resp},
	}
	requestRepo := &fakeRequestRepoForOffers{requests: map[uuid.UUID]*request.BuyerRequest{buyerReqID: req}}
	offerRepo := newFakeOfferRepo()

	svc := NewService(offerRepo, routingRepo, requestRepo, &fakeEventsForOffers{}, &fakeAuditsForOffers{}, testLogger())

	offers, err := svc.BuildForBuyerRequest(context.Background(), buyerReqID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(offers) != 1 {
		t.Fatalf("expected 1 offer, got %d", len(offers))
	}
	if offers[0].Type != TypeExact {
		t.Errorf("expected EXACT offer type, got %s", offers[0].Type)
	}
	if offers[0].RankingScore == 0 {
		t.Error("expected a non-zero ranking score")
	}
	if len(offers[0].RankingReasons) == 0 {
		t.Error("expected ranking reasons to be recorded")
	}
}

func TestBuildForBuyerRequest_UnclassifiedResponseNeverBecomesOffer(t *testing.T) {
	buyerReqID := uuid.New()
	req := &request.BuyerRequest{ID: buyerReqID, Status: request.StatusConfirmed, Product: strp("iPhone 13")}
	vr := &routing.VendorRequest{ID: uuid.New(), BuyerRequestID: buyerReqID, VendorID: uuid.New()}
	resp := &routing.VendorResponse{
		ID: uuid.New(), VendorRequestID: &vr.ID, Status: routing.ResponseUnclassified, ReviewState: routing.ReviewNeedsReview,
	}

	routingRepo := &fakeRoutingRepoForOffers{
		vendorRequests: map[uuid.UUID]*routing.VendorRequest{vr.ID: vr},
		responses:      map[uuid.UUID]*routing.VendorResponse{vr.ID: resp},
	}
	requestRepo := &fakeRequestRepoForOffers{requests: map[uuid.UUID]*request.BuyerRequest{buyerReqID: req}}
	svc := NewService(newFakeOfferRepo(), routingRepo, requestRepo, &fakeEventsForOffers{}, &fakeAuditsForOffers{}, testLogger())

	offers, err := svc.BuildForBuyerRequest(context.Background(), buyerReqID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(offers) != 0 {
		t.Fatalf("UNCLASSIFIED response must never become a buyer-facing offer, got %d offers", len(offers))
	}
}

func TestBuildForBuyerRequest_NeedsReviewResponseNeverBecomesOffer(t *testing.T) {
	// Even a confidently-classified AVAILABLE response must not become an
	// offer while it still needs human review (e.g. unmatched association).
	buyerReqID := uuid.New()
	req := &request.BuyerRequest{ID: buyerReqID, Status: request.StatusConfirmed, Product: strp("iPhone 13")}
	vr := &routing.VendorRequest{ID: uuid.New(), BuyerRequestID: buyerReqID, VendorID: uuid.New()}
	resp := &routing.VendorResponse{
		ID: uuid.New(), VendorRequestID: &vr.ID, Status: routing.ResponseAvailable, ReviewState: routing.ReviewNeedsReview,
		Product: strp("iPhone 13"),
	}
	routingRepo := &fakeRoutingRepoForOffers{
		vendorRequests: map[uuid.UUID]*routing.VendorRequest{vr.ID: vr},
		responses:      map[uuid.UUID]*routing.VendorResponse{vr.ID: resp},
	}
	requestRepo := &fakeRequestRepoForOffers{requests: map[uuid.UUID]*request.BuyerRequest{buyerReqID: req}}
	svc := NewService(newFakeOfferRepo(), routingRepo, requestRepo, &fakeEventsForOffers{}, &fakeAuditsForOffers{}, testLogger())

	offers, _ := svc.BuildForBuyerRequest(context.Background(), buyerReqID)
	if len(offers) != 0 {
		t.Fatalf("NEEDS_REVIEW response must not become an offer, got %d", len(offers))
	}
}

func TestBuildForBuyerRequest_UnavailableResponseNeverBecomesOffer(t *testing.T) {
	buyerReqID := uuid.New()
	req := &request.BuyerRequest{ID: buyerReqID, Status: request.StatusConfirmed, Product: strp("iPhone 13")}
	vr := &routing.VendorRequest{ID: uuid.New(), BuyerRequestID: buyerReqID, VendorID: uuid.New()}
	resp := &routing.VendorResponse{
		ID: uuid.New(), VendorRequestID: &vr.ID, Status: routing.ResponseUnavailable, ReviewState: routing.ReviewReviewed,
	}
	routingRepo := &fakeRoutingRepoForOffers{
		vendorRequests: map[uuid.UUID]*routing.VendorRequest{vr.ID: vr},
		responses:      map[uuid.UUID]*routing.VendorResponse{vr.ID: resp},
	}
	requestRepo := &fakeRequestRepoForOffers{requests: map[uuid.UUID]*request.BuyerRequest{buyerReqID: req}}
	svc := NewService(newFakeOfferRepo(), routingRepo, requestRepo, &fakeEventsForOffers{}, &fakeAuditsForOffers{}, testLogger())

	offers, _ := svc.BuildForBuyerRequest(context.Background(), buyerReqID)
	if len(offers) != 0 {
		t.Fatalf("a vendor saying UNAVAILABLE must never become an offer, got %d", len(offers))
	}
}

func TestBuildForBuyerRequest_AlternativeAndCanSourceRepresentedCorrectly(t *testing.T) {
	buyerReqID := uuid.New()
	req := &request.BuyerRequest{ID: buyerReqID, Status: request.StatusConfirmed, Product: strp("iPhone 13")}

	vr1 := &routing.VendorRequest{ID: uuid.New(), BuyerRequestID: buyerReqID, VendorID: uuid.New()}
	resp1 := &routing.VendorResponse{ID: uuid.New(), VendorRequestID: &vr1.ID, Status: routing.ResponseAlternative, ReviewState: routing.ReviewReviewed, Variant: strp("256GB"), Price: i64p(700000)}

	vr2 := &routing.VendorRequest{ID: uuid.New(), BuyerRequestID: buyerReqID, VendorID: uuid.New()}
	resp2 := &routing.VendorResponse{ID: uuid.New(), VendorRequestID: &vr2.ID, Status: routing.ResponseCanSource, ReviewState: routing.ReviewReviewed, Price: i64p(640000)}

	routingRepo := &fakeRoutingRepoForOffers{
		vendorRequests: map[uuid.UUID]*routing.VendorRequest{vr1.ID: vr1, vr2.ID: vr2},
		responses:      map[uuid.UUID]*routing.VendorResponse{vr1.ID: resp1, vr2.ID: resp2},
	}
	requestRepo := &fakeRequestRepoForOffers{requests: map[uuid.UUID]*request.BuyerRequest{buyerReqID: req}}
	svc := NewService(newFakeOfferRepo(), routingRepo, requestRepo, &fakeEventsForOffers{}, &fakeAuditsForOffers{}, testLogger())

	offers, _ := svc.BuildForBuyerRequest(context.Background(), buyerReqID)
	if len(offers) != 2 {
		t.Fatalf("expected 2 offers, got %d", len(offers))
	}
	var foundAlt, foundCanSource bool
	for _, o := range offers {
		if o.Type == TypeAlternative {
			foundAlt = true
		}
		if o.Type == TypeCanSource {
			foundCanSource = true
		}
	}
	if !foundAlt || !foundCanSource {
		t.Errorf("expected both ALTERNATIVE and CAN_SOURCE offer types, got alt=%v can_source=%v", foundAlt, foundCanSource)
	}
}

func TestBuildForBuyerRequest_IdempotentOnRepeatedCalls(t *testing.T) {
	buyerReqID := uuid.New()
	req := &request.BuyerRequest{ID: buyerReqID, Status: request.StatusConfirmed, Product: strp("iPhone 13")}
	vr := &routing.VendorRequest{ID: uuid.New(), BuyerRequestID: buyerReqID, VendorID: uuid.New()}
	resp := &routing.VendorResponse{ID: uuid.New(), VendorRequestID: &vr.ID, Status: routing.ResponseAvailable, ReviewState: routing.ReviewReviewed, Product: strp("iPhone 13")}

	routingRepo := &fakeRoutingRepoForOffers{
		vendorRequests: map[uuid.UUID]*routing.VendorRequest{vr.ID: vr},
		responses:      map[uuid.UUID]*routing.VendorResponse{vr.ID: resp},
	}
	requestRepo := &fakeRequestRepoForOffers{requests: map[uuid.UUID]*request.BuyerRequest{buyerReqID: req}}
	svc := NewService(newFakeOfferRepo(), routingRepo, requestRepo, &fakeEventsForOffers{}, &fakeAuditsForOffers{}, testLogger())

	svc.BuildForBuyerRequest(context.Background(), buyerReqID)
	offers, _ := svc.BuildForBuyerRequest(context.Background(), buyerReqID) // call again

	if len(offers) != 1 {
		t.Fatalf("expected exactly 1 offer after calling twice (idempotent), got %d", len(offers))
	}
}

func TestRankOffer_DeterministicAcrossRuns(t *testing.T) {
	req := &request.BuyerRequest{Product: strp("iPhone 13"), Variant: strp("128GB"), BudgetMax: i64p(700000), Location: strp("Benin City")}
	resp := &routing.VendorResponse{Product: strp("iPhone 13"), Variant: strp("128GB"), Price: i64p(650000), Location: strp("Benin City"), Status: routing.ResponseAvailable}

	score1, reasons1, _ := rankOffer(req, resp)
	score2, reasons2, _ := rankOffer(req, resp)

	if score1 != score2 {
		t.Errorf("expected deterministic score, got %d and %d", score1, score2)
	}
	if len(reasons1) != len(reasons2) {
		t.Errorf("expected deterministic reasons, got %v and %v", reasons1, reasons2)
	}
}

func TestFormatBuyerMessage_NoInternalMetadataLeaked(t *testing.T) {
	offers := []*Offer{
		{Price: i64p(620000), Type: TypeExact, RankingScore: 90, Confidence: 0.95},
	}
	msg := FormatBuyerMessage("iPhone 13", offers)

	if strings_Contains(msg, "90") || strings_Contains(msg, "0.95") || strings_Contains(msg, "Confidence") || strings_Contains(msg, "Score") {
		t.Errorf("buyer-facing message must not leak internal scores/confidence, got: %s", msg)
	}
	if !strings_Contains(msg, "620,000") {
		t.Errorf("expected formatted price in message, got: %s", msg)
	}
}

func strings_Contains(s, substr string) bool {
	return len(s) >= len(substr) && (func() bool {
		for i := 0; i+len(substr) <= len(s); i++ {
			if s[i:i+len(substr)] == substr {
				return true
			}
		}
		return false
	})()
}
