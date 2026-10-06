package request

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
)

// --- in-memory fakes for testing the Service in isolation ---

type fakeRepo struct {
	byID map[uuid.UUID]*BuyerRequest
}

func newFakeRepo() *fakeRepo { return &fakeRepo{byID: map[uuid.UUID]*BuyerRequest{}} }

func (f *fakeRepo) Create(ctx context.Context, r *BuyerRequest) (*BuyerRequest, error) {
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	f.byID[r.ID] = r
	return r, nil
}
func (f *fakeRepo) FindByID(ctx context.Context, id uuid.UUID) (*BuyerRequest, error) {
	r := f.byID[id]
	if r == nil {
		return nil, nil
	}
	cp := *r // return a copy — real (Postgres) repos always return a fresh scan, never a shared pointer
	return &cp, nil
}
func (f *fakeRepo) List(ctx context.Context, statusFilter *Status) ([]*BuyerRequest, error) {
	var out []*BuyerRequest
	for _, r := range f.byID {
		out = append(out, r)
	}
	return out, nil
}
func (f *fakeRepo) UpdateExtraction(ctx context.Context, id uuid.UUID, fields Fields, extractedData map[string]any, confidence float64, newStatus Status) (*BuyerRequest, error) {
	r := f.byID[id]
	r.Category, r.Product, r.Variant = fields.Category, fields.Product, fields.Variant
	r.BudgetMin, r.BudgetMax = fields.BudgetMin, fields.BudgetMax
	r.Location, r.Condition, r.Quantity, r.Urgency = fields.Location, fields.Condition, fields.Quantity, fields.Urgency
	r.ExtractedData = extractedData
	r.Confidence = confidence
	r.Status = newStatus
	r.Completeness = r.CalculateCompleteness()
	return r, nil
}
func (f *fakeRepo) UpdateFields(ctx context.Context, id uuid.UUID, fields Fields) (*BuyerRequest, error) {
	r := f.byID[id]
	if fields.Category != nil {
		r.Category = fields.Category
	}
	if fields.Product != nil {
		r.Product = fields.Product
	}
	if fields.BudgetMin != nil {
		r.BudgetMin = fields.BudgetMin
	}
	if fields.BudgetMax != nil {
		r.BudgetMax = fields.BudgetMax
	}
	if fields.Location != nil {
		r.Location = fields.Location
	}
	r.Completeness = r.CalculateCompleteness()
	return r, nil
}
func (f *fakeRepo) Confirm(ctx context.Context, id uuid.UUID) (*BuyerRequest, error) {
	r := f.byID[id]
	r.Status = StatusConfirmed
	return r, nil
}
func (f *fakeRepo) FindLatestOpenByConversation(ctx context.Context, conversationID uuid.UUID) (*BuyerRequest, error) {
	return nil, nil
}

type fakeConvRepo struct{}

func (f *fakeConvRepo) FindOrCreateByExternalID(ctx context.Context, channel, externalID string) (*conversation.Conversation, error) {
	return &conversation.Conversation{ID: uuid.New(), ExternalID: externalID}, nil
}
func (f *fakeConvRepo) FindByID(ctx context.Context, id uuid.UUID) (*conversation.Conversation, error) {
	return &conversation.Conversation{ID: id, ExternalID: "2348000000000"}, nil
}
func (f *fakeConvRepo) SaveMessage(ctx context.Context, msg *conversation.Message) (*conversation.Message, error) {
	return msg, nil
}
func (f *fakeConvRepo) MessageExists(ctx context.Context, conversationID uuid.UUID, externalMessageID string) (bool, error) {
	return false, nil
}
func (f *fakeConvRepo) LinkBuyer(ctx context.Context, conversationID, buyerID uuid.UUID) error {
	return nil
}

type fakeAIProvider struct {
	result *ai.ExtractionResult
	err    error
}

func (f *fakeAIProvider) Extract(ctx context.Context, req ai.ExtractionRequest) (*ai.ExtractionResult, error) {
	return f.result, f.err
}
func (f *fakeAIProvider) ExtractSupply(ctx context.Context, text string) (*ai.SupplyExtractionResult, error) {
	return &ai.SupplyExtractionResult{}, nil
}
func (f *fakeAIProvider) ExtractVendorResponse(ctx context.Context, text string) (*ai.VendorResponseExtractionResult, error) {
	return &ai.VendorResponseExtractionResult{}, nil
}
func (f *fakeAIProvider) Healthy(ctx context.Context) error { return nil }

type fakeEventWriter struct {
	events []*event.Event
}

func (f *fakeEventWriter) Write(ctx context.Context, e *event.Event) error {
	f.events = append(f.events, e)
	return nil
}

type fakeAuditWriter struct {
	entries []*audit.Entry
}

func (f *fakeAuditWriter) Write(ctx context.Context, e *audit.Entry) error {
	f.entries = append(f.entries, e)
	return nil
}

type fakeMessaging struct {
	sent []string
}

func (f *fakeMessaging) SendText(ctx context.Context, to, body string) error {
	f.sent = append(f.sent, body)
	return nil
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// --- tests ---

func TestServiceCorrect_RecordsAuditDiffForChangedFieldsOnly(t *testing.T) {
	repo := newFakeRepo()
	events := &fakeEventWriter{}
	audits := &fakeAuditWriter{}

	svc := NewService(repo, &fakeConvRepo{}, &fakeAIProvider{}, events, audits, &fakeMessaging{}, testLogger())

	ctx := context.Background()
	created, _ := repo.Create(ctx, &BuyerRequest{
		Location: strPtr("Lagos"),
		Product:  strPtr("iPhone 13"),
	})

	newLocation := "Benin City"
	updated, err := svc.Correct(ctx, created.ID, Fields{Location: &newLocation}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if *updated.Location != "Benin City" {
		t.Errorf("expected location updated to Benin City, got %v", updated.Location)
	}
	if *updated.Product != "iPhone 13" {
		t.Errorf("product should be unaffected by an unrelated correction, got %v", updated.Product)
	}

	if len(audits.entries) != 1 {
		t.Fatalf("expected exactly 1 audit entry, got %d", len(audits.entries))
	}
	entry := audits.entries[0]
	if entry.Action != "REQUEST_CORRECTED" {
		t.Errorf("expected action REQUEST_CORRECTED, got %s", entry.Action)
	}
	if entry.PreviousValue["location"] != "Lagos" {
		t.Errorf("expected previous location 'Lagos' in audit, got %v", entry.PreviousValue["location"])
	}
	if entry.NewValue["location"] != "Benin City" {
		t.Errorf("expected new location 'Benin City' in audit, got %v", entry.NewValue["location"])
	}
	if _, present := entry.PreviousValue["product"]; present {
		t.Errorf("product should not appear in the diff since it wasn't changed")
	}

	foundCorrected := false
	for _, e := range events.events {
		if e.EventType == event.RequestCorrected {
			foundCorrected = true
		}
	}
	if !foundCorrected {
		t.Error("expected a REQUEST_CORRECTED event to be written")
	}
}

func TestServiceCorrect_NoOpWhenValueUnchanged(t *testing.T) {
	repo := newFakeRepo()
	audits := &fakeAuditWriter{}
	svc := NewService(repo, &fakeConvRepo{}, &fakeAIProvider{}, &fakeEventWriter{}, audits, &fakeMessaging{}, testLogger())

	ctx := context.Background()
	created, _ := repo.Create(ctx, &BuyerRequest{Location: strPtr("Lagos")})

	sameLocation := "Lagos"
	_, err := svc.Correct(ctx, created.ID, Fields{Location: &sameLocation}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(audits.entries) != 0 {
		t.Errorf("expected no audit entry when correction doesn't change the value, got %d", len(audits.entries))
	}
}

func TestServiceRunExtraction_AIFailureDegradesGracefully(t *testing.T) {
	repo := newFakeRepo()
	events := &fakeEventWriter{}

	failingAI := &fakeAIProvider{err: context.DeadlineExceeded}
	svc := NewService(repo, &fakeConvRepo{}, failingAI, events, &fakeAuditWriter{}, &fakeMessaging{}, testLogger())

	ctx := context.Background()
	created, _ := repo.Create(ctx, &BuyerRequest{
		Status:     StatusExtracting,
		RawMessage: "need a laptop",
	})

	svc.RunExtraction(ctx, created.ID)

	updated, _ := repo.FindByID(ctx, created.ID)
	if updated.Status != StatusNeedsReview {
		t.Errorf("expected status NEEDS_REVIEW after AI failure, got %s", updated.Status)
	}
	if updated.ExtractedData["error"] == nil {
		t.Error("expected extracted_data to record the AI failure reason")
	}

	// Critically: the request must still exist and be reviewable — AI
	// failure must never lose the buyer's message (PRD 1 §20).
	if updated.RawMessage != "need a laptop" {
		t.Error("raw message must be preserved even when extraction fails")
	}
}

func TestServiceRunExtraction_SuccessUpdatesFieldsAndStatus(t *testing.T) {
	repo := newFakeRepo()
	events := &fakeEventWriter{}

	product := "iPhone 13"
	budget := int64(400000)
	successAI := &fakeAIProvider{result: &ai.ExtractionResult{
		Product:    &product,
		BudgetMax:  &budget,
		Confidence: 0.9,
	}}
	svc := NewService(repo, &fakeConvRepo{}, successAI, events, &fakeAuditWriter{}, &fakeMessaging{}, testLogger())

	ctx := context.Background()
	created, _ := repo.Create(ctx, &BuyerRequest{Status: StatusExtracting, RawMessage: "need an iphone 13, 400k"})

	svc.RunExtraction(ctx, created.ID)

	updated, _ := repo.FindByID(ctx, created.ID)
	if updated.Status != StatusNeedsReview {
		t.Errorf("expected NEEDS_REVIEW, got %s", updated.Status)
	}
	if updated.Product == nil || *updated.Product != "iPhone 13" {
		t.Errorf("expected product extracted, got %v", updated.Product)
	}
	if updated.Confidence != 0.9 {
		t.Errorf("expected confidence 0.9, got %f", updated.Confidence)
	}
}

func TestServiceClarify_SendsQuestionAndRecordsEvent(t *testing.T) {
	repo := newFakeRepo()
	events := &fakeEventWriter{}
	messaging := &fakeMessaging{}

	svc := NewService(repo, &fakeConvRepo{}, &fakeAIProvider{}, events, &fakeAuditWriter{}, messaging, testLogger())

	ctx := context.Background()
	created, _ := repo.Create(ctx, &BuyerRequest{ConversationID: uuid.New()})

	if err := svc.Clarify(ctx, created.ID, "What's your budget?"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(messaging.sent) != 1 || messaging.sent[0] != "What's your budget?" {
		t.Errorf("expected clarification question sent, got %v", messaging.sent)
	}

	found := false
	for _, e := range events.events {
		if e.EventType == event.ClarificationSent {
			found = true
		}
	}
	if !found {
		t.Error("expected a CLARIFICATION_SENT event")
	}
}

func (r *fakeRepo) FindDiscoveryAsk(ctx context.Context, deliveryID, listingID uuid.UUID) (*BuyerRequest, error) {
	return nil, nil
}
