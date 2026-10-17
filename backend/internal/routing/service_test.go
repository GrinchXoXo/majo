package routing

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/vendor"
)

// --- in-memory fakes ---

type fakeRoutingRepo struct {
	vendorRequests map[uuid.UUID]*VendorRequest
	responses      map[uuid.UUID]*VendorResponse
}

func newFakeRoutingRepo() *fakeRoutingRepo {
	return &fakeRoutingRepo{
		vendorRequests: map[uuid.UUID]*VendorRequest{},
		responses:      map[uuid.UUID]*VendorResponse{},
	}
}

func (f *fakeRoutingRepo) CreateVendorRequest(ctx context.Context, vr *VendorRequest) (*VendorRequest, error) {
	if vr.ID == uuid.Nil {
		vr.ID = uuid.New()
	}
	cp := *vr
	f.vendorRequests[vr.ID] = &cp
	return &cp, nil
}
func (f *fakeRoutingRepo) MarkSent(ctx context.Context, id uuid.UUID) error {
	if vr, ok := f.vendorRequests[id]; ok {
		vr.Status = VendorRequestSent
	}
	return nil
}
func (f *fakeRoutingRepo) MarkSendFailed(ctx context.Context, id uuid.UUID, sendErr string) error {
	if vr, ok := f.vendorRequests[id]; ok {
		vr.Status = VendorRequestSendFailed
		vr.SendError = &sendErr
	}
	return nil
}
func (f *fakeRoutingRepo) FindVendorRequestByID(ctx context.Context, id uuid.UUID) (*VendorRequest, error) {
	vr, ok := f.vendorRequests[id]
	if !ok {
		return nil, nil
	}
	cp := *vr
	return &cp, nil
}
func (f *fakeRoutingRepo) FindVendorRequestByPublicReference(ctx context.Context, ref string) (*VendorRequest, error) {
	for _, vr := range f.vendorRequests {
		if vr.PublicReference == ref {
			cp := *vr
			return &cp, nil
		}
	}
	return nil, nil
}
func (f *fakeRoutingRepo) ListVendorRequestsByBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*VendorRequest, error) {
	var out []*VendorRequest
	for _, vr := range f.vendorRequests {
		if vr.BuyerRequestID == buyerRequestID {
			cp := *vr
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (f *fakeRoutingRepo) ListActiveByVendor(ctx context.Context, vendorID uuid.UUID) ([]*VendorRequest, error) {
	var out []*VendorRequest
	for _, vr := range f.vendorRequests {
		if vr.VendorID == vendorID && vr.Status == VendorRequestSent {
			cp := *vr
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (f *fakeRoutingRepo) ListByVendor(ctx context.Context, vendorID uuid.UUID) ([]*VendorRequest, error) {
	var out []*VendorRequest
	for _, vr := range f.vendorRequests {
		if vr.VendorID == vendorID {
			cp := *vr
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (f *fakeRoutingRepo) CreateVendorResponse(ctx context.Context, resp *VendorResponse) (*VendorResponse, error) {
	if resp.ID == uuid.Nil {
		resp.ID = uuid.New()
	}
	if resp.ExtractedData == nil {
		resp.ExtractedData = map[string]any{}
	}
	if resp.CreatedAt.IsZero() {
		resp.CreatedAt = time.Now()
	}
	cp := *resp
	f.responses[resp.ID] = &cp
	if resp.VendorRequestID != nil {
		if vr, ok := f.vendorRequests[*resp.VendorRequestID]; ok {
			vr.Status = VendorRequestResponded
		}
	}
	out := *resp
	return &out, nil
}
func (f *fakeRoutingRepo) UpdateVendorResponse(ctx context.Context, id uuid.UUID, fields ResponseFields) (*VendorResponse, error) {
	r, ok := f.responses[id]
	if !ok {
		return nil, nil
	}
	if fields.Status != nil {
		r.Status = *fields.Status
	}
	if fields.ReviewState != nil {
		r.ReviewState = *fields.ReviewState
	}
	if fields.Product != nil {
		r.Product = fields.Product
	}
	if fields.Variant != nil {
		r.Variant = fields.Variant
	}
	if fields.Condition != nil {
		r.Condition = fields.Condition
	}
	if fields.Price != nil {
		r.Price = fields.Price
	}
	if fields.Quantity != nil {
		r.Quantity = fields.Quantity
	}
	if fields.Location != nil {
		r.Location = fields.Location
	}
	if fields.Notes != nil {
		r.Notes = fields.Notes
	}
	if fields.ExtractionMethod != nil {
		r.ExtractionMethod = *fields.ExtractionMethod
	}
	cp := *r
	return &cp, nil
}
func (f *fakeRoutingRepo) FindResponseByID(ctx context.Context, id uuid.UUID) (*VendorResponse, error) {
	r, ok := f.responses[id]
	if !ok {
		return nil, nil
	}
	cp := *r
	return &cp, nil
}
func (f *fakeRoutingRepo) FindResponseByVendorRequest(ctx context.Context, vendorRequestID uuid.UUID) (*VendorResponse, error) {
	for _, r := range f.responses {
		if r.VendorRequestID != nil && *r.VendorRequestID == vendorRequestID {
			cp := *r
			return &cp, nil
		}
	}
	return nil, nil
}
func (f *fakeRoutingRepo) ListUnmatched(ctx context.Context) ([]*VendorResponse, error) {
	var out []*VendorResponse
	for _, r := range f.responses {
		if r.VendorRequestID == nil {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (f *fakeRoutingRepo) UpdateAssociation(ctx context.Context, responseID uuid.UUID, newVendorRequestID uuid.UUID) (*uuid.UUID, *VendorResponse, error) {
	r, ok := f.responses[responseID]
	if !ok {
		return nil, nil, nil
	}
	previous := r.VendorRequestID
	r.VendorRequestID = &newVendorRequestID
	if vr, ok := f.vendorRequests[newVendorRequestID]; ok {
		vr.Status = VendorRequestResponded
	}
	cp := *r
	return previous, &cp, nil
}

type fakeVendorRepo struct {
	vendors map[uuid.UUID]*vendor.Vendor
}

func newFakeVendorRepo() *fakeVendorRepo {
	return &fakeVendorRepo{vendors: map[uuid.UUID]*vendor.Vendor{}}
}
func (f *fakeVendorRepo) Create(ctx context.Context, v *vendor.Vendor) (*vendor.Vendor, error) {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	f.vendors[v.ID] = v
	return v, nil
}
func (f *fakeVendorRepo) Update(ctx context.Context, id uuid.UUID, fields vendor.Fields) (*vendor.Vendor, error) {
	if v := f.vendors[id]; v != nil && fields.Status != nil {
		v.Status = *fields.Status
	}
	return f.vendors[id], nil
}
func (f *fakeVendorRepo) FindByID(ctx context.Context, id uuid.UUID) (*vendor.Vendor, error) {
	return f.vendors[id], nil
}
func (f *fakeVendorRepo) FindByWhatsAppID(ctx context.Context, whatsappID string) (*vendor.Vendor, error) {
	for _, v := range f.vendors {
		if v.WhatsAppExternalID == whatsappID {
			return v, nil
		}
	}
	return nil, nil
}
func (f *fakeVendorRepo) List(ctx context.Context, statusFilter *vendor.Status, categoryFilter *string) ([]*vendor.Vendor, error) {
	var out []*vendor.Vendor
	for _, v := range f.vendors {
		out = append(out, v)
	}
	return out, nil
}
func (f *fakeVendorRepo) ActiveByCategory(ctx context.Context, category string) ([]*vendor.Vendor, error) {
	return f.List(ctx, nil, nil)
}
func (f *fakeVendorRepo) RecordMessageActivity(ctx context.Context, vendorID uuid.UUID) error {
	return nil
}
func (f *fakeVendorRepo) RecordSupplySubmission(ctx context.Context, vendorID uuid.UUID) error {
	return nil
}
func (f *fakeVendorRepo) RecordResponseActivity(ctx context.Context, vendorID uuid.UUID) error {
	return nil
}
func (f *fakeVendorRepo) RecordRequestSent(ctx context.Context, vendorID uuid.UUID) error { return nil }

type fakeSupplyRepo struct{ created []*supply.Signal }

func (f *fakeSupplyRepo) FindByID(ctx context.Context, id uuid.UUID) (*supply.Signal, error) {
	for _, s := range f.created {
		if s.ID == id {
			return s, nil
		}
	}
	return nil, nil
}

func (f *fakeSupplyRepo) Create(ctx context.Context, s *supply.Signal) (*supply.Signal, error) {
	if s.ID == uuid.Nil {
		s.ID = uuid.New()
	}
	f.created = append(f.created, s)
	return s, nil
}
func (f *fakeSupplyRepo) ListByVendor(ctx context.Context, vendorID uuid.UUID) ([]*supply.Signal, error) {
	return nil, nil
}
func (f *fakeSupplyRepo) FreshByNormalizedProduct(ctx context.Context, normalizedProduct string) ([]*supply.Signal, error) {
	return nil, nil
}
func (f *fakeSupplyRepo) FreshByVendor(ctx context.Context, vendorID uuid.UUID) ([]*supply.Signal, error) {
	return nil, nil
}
func (f *fakeSupplyRepo) Update(ctx context.Context, id uuid.UUID, fields supply.SignalFields) (*supply.Signal, error) {
	return nil, nil
}

type fakeRequestRepo struct{}

func (f *fakeRequestRepo) Create(ctx context.Context, r *request.BuyerRequest) (*request.BuyerRequest, error) {
	return r, nil
}
func (f *fakeRequestRepo) FindByID(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error) {
	return &request.BuyerRequest{ID: id}, nil
}
func (f *fakeRequestRepo) List(ctx context.Context, statusFilter *request.Status) ([]*request.BuyerRequest, error) {
	return nil, nil
}
func (f *fakeRequestRepo) UpdateExtraction(ctx context.Context, id uuid.UUID, fields request.Fields, extractedData map[string]any, confidence float64, newStatus request.Status) (*request.BuyerRequest, error) {
	return nil, nil
}
func (f *fakeRequestRepo) UpdateFields(ctx context.Context, id uuid.UUID, fields request.Fields) (*request.BuyerRequest, error) {
	return nil, nil
}
func (f *fakeRequestRepo) Confirm(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error) {
	return nil, nil
}
func (f *fakeRequestRepo) FindLatestOpenByConversation(ctx context.Context, conversationID uuid.UUID) (*request.BuyerRequest, error) {
	return nil, nil
}

// fakeAI lets each test script canned extraction results/errors.
type fakeAI struct {
	vendorResponseResult *ai.VendorResponseExtractionResult
	vendorResponseErr    error
	supplyResult         *ai.SupplyExtractionResult
}

func (f *fakeAI) Extract(ctx context.Context, req ai.ExtractionRequest) (*ai.ExtractionResult, error) {
	return &ai.ExtractionResult{}, nil
}
func (f *fakeAI) ExtractSupply(ctx context.Context, text string) (*ai.SupplyExtractionResult, error) {
	if f.supplyResult != nil {
		return f.supplyResult, nil
	}
	return &ai.SupplyExtractionResult{AvailabilityStatus: "AVAILABLE"}, nil
}
func (f *fakeAI) ExtractVendorResponse(ctx context.Context, text string) (*ai.VendorResponseExtractionResult, error) {
	return f.vendorResponseResult, f.vendorResponseErr
}
func (f *fakeAI) Healthy(ctx context.Context) error { return nil }

type fakeEvents struct{ events []*event.Event }

func (f *fakeEvents) Write(ctx context.Context, e *event.Event) error {
	f.events = append(f.events, e)
	return nil
}

type fakeAudits struct{ entries []*audit.Entry }

func (f *fakeAudits) Write(ctx context.Context, e *audit.Entry) error {
	f.entries = append(f.entries, e)
	return nil
}

type fakeMessaging struct{ sent []string }

func (f *fakeMessaging) SendText(ctx context.Context, to, body string) error {
	f.sent = append(f.sent, body)
	return nil
}

func testLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func strp(s string) *string { return &s }

// --- Association priority tests (PRD 2.1 §4) ---

func TestProcessVendorMessage_ExplicitReferenceAttachesCorrectly(t *testing.T) {
	routingRepo := newFakeRoutingRepo()
	vendorRepo := newFakeVendorRepo()
	ai := &fakeAI{vendorResponseResult: &ai.VendorResponseExtractionResult{Status: "AVAILABLE", ReviewState: "REVIEWED", Confidence: 0.7}}
	svc := NewService(vendorRepo, &fakeSupplyRepo{}, routingRepo, &fakeRequestRepo{}, ai, &fakeEvents{}, &fakeAudits{}, &fakeMessaging{}, testLogger())

	v := &vendor.Vendor{ID: uuid.New(), WhatsAppExternalID: "234800", Status: vendor.StatusActive}
	vendorRepo.vendors[v.ID] = v

	iphoneReq, _ := routingRepo.CreateVendorRequest(context.Background(), &VendorRequest{VendorID: v.ID, PublicReference: "MM-7466", Status: VendorRequestSent})
	psReq, _ := routingRepo.CreateVendorRequest(context.Background(), &VendorRequest{VendorID: v.ID, PublicReference: "MM-1074", Status: VendorRequestSent})

	svc.ProcessVendorMessage(context.Background(), v, "MM-1074 yes, 780k")

	psResp, _ := routingRepo.FindResponseByVendorRequest(context.Background(), psReq.ID)
	iphoneResp, _ := routingRepo.FindResponseByVendorRequest(context.Background(), iphoneReq.ID)

	if psResp == nil {
		t.Fatal("expected the PS5 request (MM-1074) to receive the response")
	}
	if iphoneResp != nil {
		t.Fatal("expected the iPhone request (MM-7466) to receive NO response — explicit reference must override")
	}
}

func TestProcessVendorMessage_MultipleActiveNoReference_DoesNotAutoAttach(t *testing.T) {
	// This is the exact scenario from the PRD 2.1 audit that proved the
	// original implementation's misattribution bug.
	routingRepo := newFakeRoutingRepo()
	vendorRepo := newFakeVendorRepo()
	fakeAiProvider := &fakeAI{vendorResponseResult: &ai.VendorResponseExtractionResult{Status: "AVAILABLE", ReviewState: "REVIEWED", Confidence: 0.7}}
	events := &fakeEvents{}
	svc := NewService(vendorRepo, &fakeSupplyRepo{}, routingRepo, &fakeRequestRepo{}, fakeAiProvider, events, &fakeAudits{}, &fakeMessaging{}, testLogger())

	v := &vendor.Vendor{ID: uuid.New(), WhatsAppExternalID: "234800", Status: vendor.StatusActive}
	vendorRepo.vendors[v.ID] = v

	iphoneReq, _ := routingRepo.CreateVendorRequest(context.Background(), &VendorRequest{VendorID: v.ID, PublicReference: "MM-7466", Status: VendorRequestSent})
	psReq, _ := routingRepo.CreateVendorRequest(context.Background(), &VendorRequest{VendorID: v.ID, PublicReference: "MM-1074", Status: VendorRequestSent})

	svc.ProcessVendorMessage(context.Background(), v, "Yes, 780k") // no reference at all

	iphoneResp, _ := routingRepo.FindResponseByVendorRequest(context.Background(), iphoneReq.ID)
	psResp, _ := routingRepo.FindResponseByVendorRequest(context.Background(), psReq.ID)
	unmatched, _ := routingRepo.ListUnmatched(context.Background())

	if iphoneResp != nil || psResp != nil {
		t.Fatal("expected NEITHER request to receive the response — ambiguous, must not auto-attach")
	}
	if len(unmatched) != 1 {
		t.Fatalf("expected exactly 1 unmatched response, got %d", len(unmatched))
	}
	if unmatched[0].ReviewState != ReviewNeedsReview {
		t.Errorf("expected unmatched response to be NEEDS_REVIEW, got %s", unmatched[0].ReviewState)
	}
}

func TestProcessVendorMessage_SingleActiveRequestUsesIt(t *testing.T) {
	routingRepo := newFakeRoutingRepo()
	vendorRepo := newFakeVendorRepo()
	fakeAiProvider := &fakeAI{vendorResponseResult: &ai.VendorResponseExtractionResult{Status: "AVAILABLE", ReviewState: "REVIEWED", Confidence: 0.7}}
	svc := NewService(vendorRepo, &fakeSupplyRepo{}, routingRepo, &fakeRequestRepo{}, fakeAiProvider, &fakeEvents{}, &fakeAudits{}, &fakeMessaging{}, testLogger())

	v := &vendor.Vendor{ID: uuid.New(), WhatsAppExternalID: "234800", Status: vendor.StatusActive}
	vendorRepo.vendors[v.ID] = v
	onlyReq, _ := routingRepo.CreateVendorRequest(context.Background(), &VendorRequest{VendorID: v.ID, PublicReference: "MM-0001", Status: VendorRequestSent})

	svc.ProcessVendorMessage(context.Background(), v, "Yes, 500k")

	resp, _ := routingRepo.FindResponseByVendorRequest(context.Background(), onlyReq.ID)
	if resp == nil {
		t.Fatal("expected the single active request to receive the response")
	}
}

func TestProcessVendorMessage_InvalidReference_DoesNotGuess(t *testing.T) {
	routingRepo := newFakeRoutingRepo()
	vendorRepo := newFakeVendorRepo()
	fakeAiProvider := &fakeAI{vendorResponseResult: &ai.VendorResponseExtractionResult{Status: "AVAILABLE", ReviewState: "REVIEWED", Confidence: 0.7}}
	svc := NewService(vendorRepo, &fakeSupplyRepo{}, routingRepo, &fakeRequestRepo{}, fakeAiProvider, &fakeEvents{}, &fakeAudits{}, &fakeMessaging{}, testLogger())

	v := &vendor.Vendor{ID: uuid.New(), WhatsAppExternalID: "234800", Status: vendor.StatusActive}
	vendorRepo.vendors[v.ID] = v
	realReq, _ := routingRepo.CreateVendorRequest(context.Background(), &VendorRequest{VendorID: v.ID, PublicReference: "MM-0001", Status: VendorRequestSent})

	svc.ProcessVendorMessage(context.Background(), v, "MM-9999 yes, 500k") // MM-9999 doesn't exist

	resp, _ := routingRepo.FindResponseByVendorRequest(context.Background(), realReq.ID)
	if resp != nil {
		t.Fatal("expected the real request to receive NO response for an invalid reference")
	}
	unmatched, _ := routingRepo.ListUnmatched(context.Background())
	if len(unmatched) != 1 {
		t.Fatalf("expected the response to be persisted unmatched, got %d unmatched", len(unmatched))
	}
}

func TestProcessVendorMessage_ReferenceBelongingToAnotherVendor_DoesNotCrossAttach(t *testing.T) {
	routingRepo := newFakeRoutingRepo()
	vendorRepo := newFakeVendorRepo()
	fakeAiProvider := &fakeAI{vendorResponseResult: &ai.VendorResponseExtractionResult{Status: "AVAILABLE", ReviewState: "REVIEWED", Confidence: 0.7}}
	svc := NewService(vendorRepo, &fakeSupplyRepo{}, routingRepo, &fakeRequestRepo{}, fakeAiProvider, &fakeEvents{}, &fakeAudits{}, &fakeMessaging{}, testLogger())

	vendorA := &vendor.Vendor{ID: uuid.New(), WhatsAppExternalID: "234800", Status: vendor.StatusActive}
	vendorB := &vendor.Vendor{ID: uuid.New(), WhatsAppExternalID: "234811", Status: vendor.StatusActive}
	vendorRepo.vendors[vendorA.ID] = vendorA
	vendorRepo.vendors[vendorB.ID] = vendorB

	// MM-5000 belongs to vendor B, not vendor A.
	bReq, _ := routingRepo.CreateVendorRequest(context.Background(), &VendorRequest{VendorID: vendorB.ID, PublicReference: "MM-5000", Status: VendorRequestSent})

	// Vendor A cites vendor B's reference.
	svc.ProcessVendorMessage(context.Background(), vendorA, "MM-5000 yes, 500k")

	resp, _ := routingRepo.FindResponseByVendorRequest(context.Background(), bReq.ID)
	if resp != nil {
		t.Fatal("expected vendor A's message to NOT attach to vendor B's request")
	}
}

func TestProcessVendorMessage_NoActiveRequest_FallsBackToSupply(t *testing.T) {
	routingRepo := newFakeRoutingRepo()
	vendorRepo := newFakeVendorRepo()
	supplyRepo := &fakeSupplyRepo{}
	fakeAiProvider := &fakeAI{supplyResult: &ai.SupplyExtractionResult{Product: strp("iPhone 13"), AvailabilityStatus: "AVAILABLE"}}
	svc := NewService(vendorRepo, supplyRepo, routingRepo, &fakeRequestRepo{}, fakeAiProvider, &fakeEvents{}, &fakeAudits{}, &fakeMessaging{}, testLogger())

	v := &vendor.Vendor{ID: uuid.New(), WhatsAppExternalID: "234800", Status: vendor.StatusActive}
	vendorRepo.vendors[v.ID] = v

	svc.ProcessVendorMessage(context.Background(), v, "I get iPhone 13, 620k")

	if len(supplyRepo.created) != 1 {
		t.Fatalf("expected a supply signal to be created when there's no active request, got %d", len(supplyRepo.created))
	}
}

// --- Conservative classification / no-false-UNAVAILABLE-on-failure (PRD 2.1 §8) ---

func TestProcessVendorMessage_AIFailure_NeverProducesUnavailable(t *testing.T) {
	routingRepo := newFakeRoutingRepo()
	vendorRepo := newFakeVendorRepo()
	fakeAiProvider := &fakeAI{vendorResponseErr: context.DeadlineExceeded}
	svc := NewService(vendorRepo, &fakeSupplyRepo{}, routingRepo, &fakeRequestRepo{}, fakeAiProvider, &fakeEvents{}, &fakeAudits{}, &fakeMessaging{}, testLogger())

	v := &vendor.Vendor{ID: uuid.New(), WhatsAppExternalID: "234800", Status: vendor.StatusActive}
	vendorRepo.vendors[v.ID] = v
	req, _ := routingRepo.CreateVendorRequest(context.Background(), &VendorRequest{VendorID: v.ID, PublicReference: "MM-0001", Status: VendorRequestSent})

	svc.ProcessVendorMessage(context.Background(), v, "some message")

	resp, _ := routingRepo.FindResponseByVendorRequest(context.Background(), req.ID)
	if resp == nil {
		t.Fatal("expected a response to still be created despite AI failure")
	}
	if resp.Status == ResponseUnavailable {
		t.Fatal("AI failure must NEVER produce UNAVAILABLE (PRD 2.1 §8)")
	}
	if resp.Status != ResponseUnclassified {
		t.Errorf("expected UNCLASSIFIED on AI failure, got %s", resp.Status)
	}
	if resp.ReviewState != ReviewNeedsReview {
		t.Errorf("expected NEEDS_REVIEW on AI failure, got %s", resp.ReviewState)
	}
}

// --- Correction tests (PRD 2.1 §7/§12) ---

func TestCorrectResponse_UpdatesFieldsPreservesRawMessageSetsCorrected(t *testing.T) {
	routingRepo := newFakeRoutingRepo()
	audits := &fakeAudits{}
	svc := NewService(newFakeVendorRepo(), &fakeSupplyRepo{}, routingRepo, &fakeRequestRepo{}, &fakeAI{}, &fakeEvents{}, audits, &fakeMessaging{}, testLogger())

	respID := uuid.New()
	original := int64(620000)
	routingRepo.responses[respID] = &VendorResponse{
		ID: respID, VendorID: uuid.New(), Status: ResponseAvailable, ReviewState: ReviewReviewed,
		Price: &original, RawMessage: "Yes, 620k.", ExtractionMethod: ExtractionHeuristic,
		ExtractedData: map[string]any{"price": 620000},
	}

	newPrice := int64(650000)
	updated, err := svc.CorrectResponse(context.Background(), respID, ResponseFields{Price: &newPrice}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if updated.RawMessage != "Yes, 620k." {
		t.Error("raw_message must remain unchanged after correction")
	}
	if *updated.Price != 650000 {
		t.Errorf("expected corrected price 650000, got %d", *updated.Price)
	}
	if updated.ReviewState != ReviewCorrected {
		t.Errorf("expected review_state CORRECTED after operator edit, got %s", updated.ReviewState)
	}
	if updated.ExtractionMethod != ExtractionManual {
		t.Errorf("expected extraction_method manual after a field correction, got %s", updated.ExtractionMethod)
	}

	if len(audits.entries) != 1 {
		t.Fatalf("expected exactly 1 audit entry, got %d", len(audits.entries))
	}
	entry := audits.entries[0]
	if entry.Action != "VENDOR_RESPONSE_CORRECTED" {
		t.Errorf("expected action VENDOR_RESPONSE_CORRECTED, got %s", entry.Action)
	}
	if entry.PreviousValue["price"] != int64(620000) {
		t.Errorf("expected previous price 620000 in audit, got %v", entry.PreviousValue["price"])
	}
	if entry.NewValue["price"] != int64(650000) {
		t.Errorf("expected new price 650000 in audit, got %v", entry.NewValue["price"])
	}
}

// --- Association correction tests (PRD 2.1 §5) ---

func TestAssociateResponse_CreatesAuditWithBeforeAfter(t *testing.T) {
	routingRepo := newFakeRoutingRepo()
	audits := &fakeAudits{}
	svc := NewService(newFakeVendorRepo(), &fakeSupplyRepo{}, routingRepo, &fakeRequestRepo{}, &fakeAI{}, &fakeEvents{}, audits, &fakeMessaging{}, testLogger())

	respID, vendorID := uuid.New(), uuid.New()
	routingRepo.responses[respID] = &VendorResponse{
		ID: respID, VendorID: vendorID, Status: ResponseAvailable, ReviewState: ReviewNeedsReview,
		RawMessage: "Yes, 780k", VendorRequestID: nil, // unmatched
	}
	// Phase 3.1 §3A-3: the target request belongs to the SAME vendor (the
	// pre-3.1 fixture used a different vendor, which is now rejected).
	targetVR, _ := routingRepo.CreateVendorRequest(context.Background(), &VendorRequest{VendorID: vendorID, PublicReference: "MM-1074"})

	updated, err := svc.AssociateResponse(context.Background(), respID, targetVR.ID, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if updated.VendorRequestID == nil || *updated.VendorRequestID != targetVR.ID {
		t.Fatal("expected response to now be associated with the target vendor request")
	}

	if len(audits.entries) != 1 {
		t.Fatalf("expected 1 audit entry, got %d", len(audits.entries))
	}
	entry := audits.entries[0]
	if entry.Action != "VENDOR_RESPONSE_ASSOCIATION_CORRECTED" {
		t.Errorf("expected action VENDOR_RESPONSE_ASSOCIATION_CORRECTED, got %s", entry.Action)
	}
	if entry.PreviousValue["vendor_request_id"] != nil {
		t.Errorf("expected previous vendor_request_id nil (was unmatched), got %v", entry.PreviousValue["vendor_request_id"])
	}
	if entry.NewValue["vendor_request_id"] != targetVR.ID.String() {
		t.Errorf("expected new vendor_request_id %s, got %v", targetVR.ID, entry.NewValue["vendor_request_id"])
	}
}

// --- Phase 3.1 §3A tests ---

func TestAssociateResponse_RejectsCrossVendorAssociation(t *testing.T) {
	routingRepo := newFakeRoutingRepo()
	audits := &fakeAudits{}
	events := &fakeEvents{}
	svc := NewService(newFakeVendorRepo(), &fakeSupplyRepo{}, routingRepo, &fakeRequestRepo{}, &fakeAI{}, events, audits, &fakeMessaging{}, testLogger())

	ojID, mikeID := uuid.New(), uuid.New()
	respID := uuid.New()
	routingRepo.responses[respID] = &VendorResponse{
		ID: respID, VendorID: ojID, Status: ResponseAvailable, ReviewState: ReviewNeedsReview, RawMessage: "MM-9999 yes 650k",
	}
	mikesVR, _ := routingRepo.CreateVendorRequest(context.Background(), &VendorRequest{VendorID: mikeID, PublicReference: "MM-3991", Status: VendorRequestSent})

	_, err := svc.AssociateResponse(context.Background(), respID, mikesVR.ID, nil)
	if !errors.Is(err, ErrCrossVendorAssociation) {
		t.Fatalf("expected ErrCrossVendorAssociation, got %v", err)
	}
	if routingRepo.responses[respID].VendorRequestID != nil {
		t.Fatal("cross-vendor association must not modify the response")
	}
	if len(audits.entries) != 0 {
		t.Fatalf("no association audit should be written for a rejected attempt, got %d", len(audits.entries))
	}
	rejected := false
	for _, e := range events.events {
		if e.EventType == event.VendorResponseAssociationRejected {
			rejected = true
		}
	}
	if !rejected {
		t.Fatal("expected a VENDOR_RESPONSE_ASSOCIATION_REJECTED event for auditability")
	}
}

type failingMessaging struct{}

func (failingMessaging) SendText(ctx context.Context, to, body string) error {
	return errors.New(`whatsapp api returned status 400: {"error":{"code":131047,"message":"Re-engagement message"}}`)
}

type confirmedRequestRepo struct{ fakeRequestRepo }

func (confirmedRequestRepo) FindByID(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error) {
	p := "iphone 13"
	return &request.BuyerRequest{ID: id, Status: request.StatusConfirmed, Product: &p}, nil
}

func TestRouteToVendors_SendFailureIsDistinctAndNotActive(t *testing.T) {
	routingRepo := newFakeRoutingRepo()
	vendorRepo := newFakeVendorRepo()
	v, _ := vendorRepo.Create(context.Background(), &vendor.Vendor{BusinessName: "Mike Gadgets", WhatsAppExternalID: "2348000000001", Status: vendor.StatusActive})
	events := &fakeEvents{}
	svc := NewService(vendorRepo, &fakeSupplyRepo{}, routingRepo, &confirmedRequestRepo{}, &fakeAI{}, events, &fakeAudits{}, failingMessaging{}, testLogger())

	req, _ := (&confirmedRequestRepo{}).FindByID(context.Background(), uuid.New())
	created, err := svc.RouteToVendors(context.Background(), req, []uuid.UUID{v.ID}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(created) != 1 || created[0].Status != VendorRequestSendFailed {
		t.Fatalf("expected one SEND_FAILED vendor request, got %+v", created)
	}
	stored := routingRepo.vendorRequests[created[0].ID]
	if stored.Status != VendorRequestSendFailed || stored.SendError == nil {
		t.Fatalf("expected persisted SEND_FAILED with error detail, got status=%s err=%v", stored.Status, stored.SendError)
	}

	active, _ := routingRepo.ListActiveByVendor(context.Background(), v.ID)
	if len(active) != 0 {
		t.Fatalf("a request the vendor never received must not be awaiting a reply, got %d active", len(active))
	}

	// The vendor's next, unrelated message must be treated as supply, not
	// as a response to the request they never received.
	supplyRepo := &fakeSupplyRepo{}
	svc2 := NewService(vendorRepo, supplyRepo, routingRepo, &confirmedRequestRepo{}, &fakeAI{}, events, &fakeAudits{}, failingMessaging{}, testLogger())
	if resp := svc2.ProcessVendorMessage(context.Background(), v, "I get PS5 slim 550k"); resp != nil {
		t.Fatal("expected no VendorResponse to be created for a vendor with only a SEND_FAILED request")
	}
	if len(supplyRepo.created) != 1 {
		t.Fatalf("expected the message to become a SupplySignal, got %d", len(supplyRepo.created))
	}

	found := false
	for _, e := range events.events {
		if e.EventType == event.VendorRequestSendFailed {
			found = true
		}
	}
	if !found {
		t.Fatal("expected VENDOR_REQUEST_SEND_FAILED event")
	}
}

func TestRouteAndSuggest_RejectUnconfirmedRequest(t *testing.T) {
	svc := NewService(newFakeVendorRepo(), &fakeSupplyRepo{}, newFakeRoutingRepo(), &fakeRequestRepo{}, &fakeAI{}, &fakeEvents{}, &fakeAudits{}, &fakeMessaging{}, testLogger())

	unconfirmed := &request.BuyerRequest{ID: uuid.New(), Status: request.StatusNeedsReview}
	if _, err := svc.RouteToVendors(context.Background(), unconfirmed, []uuid.UUID{uuid.New()}, nil, nil); !errors.Is(err, request.ErrNotConfirmed) {
		t.Fatalf("RouteToVendors: expected ErrNotConfirmed, got %v", err)
	}
	// fakeRequestRepo.FindByID returns a request with empty status.
	if _, _, err := svc.SuggestVendors(context.Background(), uuid.New()); !errors.Is(err, request.ErrNotConfirmed) {
		t.Fatalf("SuggestVendors: expected ErrNotConfirmed, got %v", err)
	}
}

type collidingRoutingRepo struct {
	*fakeRoutingRepo
	collisionsLeft int
	attempts       []string
}

func (c *collidingRoutingRepo) CreateVendorRequest(ctx context.Context, vr *VendorRequest) (*VendorRequest, error) {
	c.attempts = append(c.attempts, vr.PublicReference)
	if c.collisionsLeft > 0 {
		c.collisionsLeft--
		return nil, ErrDuplicateReference
	}
	return c.fakeRoutingRepo.CreateVendorRequest(ctx, vr)
}

func TestRouteToVendors_RetriesOnReferenceCollisionAndUsesSixDigits(t *testing.T) {
	repo := &collidingRoutingRepo{fakeRoutingRepo: newFakeRoutingRepo(), collisionsLeft: 2}
	vendorRepo := newFakeVendorRepo()
	v, _ := vendorRepo.Create(context.Background(), &vendor.Vendor{BusinessName: "OJ", WhatsAppExternalID: "2348000000002", Status: vendor.StatusActive})
	svc := NewService(vendorRepo, &fakeSupplyRepo{}, repo, &confirmedRequestRepo{}, &fakeAI{}, &fakeEvents{}, &fakeAudits{}, &fakeMessaging{}, testLogger())

	req, _ := (&confirmedRequestRepo{}).FindByID(context.Background(), uuid.New())
	created, err := svc.RouteToVendors(context.Background(), req, []uuid.UUID{v.ID}, nil, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(created) != 1 {
		t.Fatalf("a reference collision must be retried, not silently skip the vendor; got %d created", len(created))
	}
	if len(repo.attempts) != 3 {
		t.Fatalf("expected 3 attempts (2 collisions + success), got %d", len(repo.attempts))
	}
	ref := created[0].PublicReference
	if !regexp.MustCompile(`^MM-\d{6}$`).MatchString(ref) {
		t.Fatalf("expected a 6-digit MM reference, got %q", ref)
	}
	// Existing 4-digit references remain parseable from vendor replies.
	if m := mmReferencePattern.FindStringSubmatch("MM-1074 yes"); m == nil || m[1] != "1074" {
		t.Fatal("legacy 4-digit references must still be recognized")
	}
}

func (r *fakeRequestRepo) FindDiscoveryAsk(ctx context.Context, deliveryID, listingID uuid.UUID) (*request.BuyerRequest, error) {
	return nil, nil
}

func (f *fakeRequestRepo) SetStatus(ctx context.Context, id uuid.UUID, from []request.Status, to request.Status) (*request.BuyerRequest, error) {
	return nil, nil
}

// Pilot batch: a headcount the buyer stated reaches the vendor.
func TestVendorRequestMessageCarriesPreferences(t *testing.T) {
	p := "small chops"
	max := int64(45000)
	msg := buildVendorRequestMessage("MM-123456", &request.BuyerRequest{Product: &p, BudgetMax: &max, Preferences: []string{"for 20 people", " "}})
	if !strings.Contains(msg, "small chops") || !strings.Contains(msg, "Up to ₦45000") || !strings.Contains(msg, "\nfor 20 people\n") {
		t.Fatalf("message: %q", msg)
	}
}
