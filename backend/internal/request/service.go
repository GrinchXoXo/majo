package request

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
)

// MessagingProvider is the minimal outbound-messaging surface this service
// needs. Defined locally (rather than importing the whatsapp package
// directly) to keep the request domain independent of any specific
// channel implementation, per PRD 0.2 §26-27.
type MessagingProvider interface {
	SendText(ctx context.Context, to, body string) error
}

// Service is the application-layer orchestrator for the BuyerRequest
// lifecycle: creation, background extraction, operator correction,
// confirmation, and clarification. This is the object Phase 1's HTTP
// handlers and webhook processing call into — it is the single place
// business rules about requests live.
type Service struct {
	requests   Repository
	convRepo   conversation.Repository
	aiProvider ai.Provider
	events     event.Writer
	audits     audit.Writer
	messaging  MessagingProvider
	logger     *slog.Logger
}

func NewService(
	requests Repository,
	convRepo conversation.Repository,
	aiProvider ai.Provider,
	events event.Writer,
	audits audit.Writer,
	messaging MessagingProvider,
	logger *slog.Logger,
) *Service {
	return &Service{
		requests:   requests,
		convRepo:   convRepo,
		aiProvider: aiProvider,
		events:     events,
		audits:     audits,
		messaging:  messaging,
		logger:     logger,
	}
}

// NewRequest is the single entry point for demand (Architecture Lock V2
// §E: Request.Create(origin, buyer, structured_fields?, raw_text?,
// listing_id?, discovery_delivery_id?)).
type NewRequest struct {
	Origin          Origin
	ConversationID  uuid.UUID
	BuyerID         uuid.UUID
	OriginMessageID *uuid.UUID
	RawText         string
	// Fields are structured fields known at creation (listing-origin demand
	// copies them from a curated listing, so no extraction runs).
	Fields *Fields
	// ListingID / DiscoveryDeliveryID attribute listing-origin demand.
	ListingID           *uuid.UUID
	DiscoveryDeliveryID *uuid.UUID
	// ExtractedData records where the structured fields came from.
	ExtractedData map[string]any
	// EventExtra is merged into the REQUEST_CREATED payload (attribution).
	EventExtra map[string]any
}

// Create creates a BuyerRequest for any origin. Message-origin demand
// starts EXTRACTING (extraction runs separately); listing-origin demand
// arrives with structured fields and starts NEEDS_REVIEW. Confirmation is
// never implied: only Confirm makes a request CONFIRMED.
//
// Listing-origin creation is idempotent per (delivery, listing): a repeat
// returns the existing request with created=false and writes no event.
func (s *Service) Create(ctx context.Context, n NewRequest) (req *BuyerRequest, created bool, err error) {
	if n.Origin == "" {
		n.Origin = OriginMessage
	}
	r := &BuyerRequest{
		ConversationID: n.ConversationID, BuyerID: n.BuyerID, OriginMessageID: n.OriginMessageID,
		Origin: n.Origin, ListingID: n.ListingID, DiscoveryDeliveryID: n.DiscoveryDeliveryID,
		Status: StatusExtracting, RawMessage: n.RawText, Currency: "NGN", ExtractedData: n.ExtractedData,
	}
	if n.Origin != OriginMessage {
		if n.ListingID == nil || n.Fields == nil {
			return nil, false, fmt.Errorf("listing-origin request needs a listing and its fields")
		}
		f := n.Fields
		r.Category, r.Product, r.Variant, r.Condition = f.Category, f.Product, f.Variant, f.Condition
		r.BudgetMin, r.BudgetMax, r.Location, r.Quantity = f.BudgetMin, f.BudgetMax, f.Location, f.Quantity
		r.Status = StatusNeedsReview
		r.Confidence = 1
		r.Completeness = r.CalculateCompleteness()
	}

	saved, err := s.requests.Create(ctx, r)
	if errors.Is(err, ErrDuplicate) && n.DiscoveryDeliveryID != nil && n.ListingID != nil {
		existing, ferr := s.requests.FindDiscoveryAsk(ctx, *n.DiscoveryDeliveryID, *n.ListingID)
		if ferr != nil || existing == nil {
			return nil, false, fmt.Errorf("loading existing discovery request: %v", ferr)
		}
		return existing, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("creating buyer request: %w", err)
	}

	channel := ""
	if conv, err := s.convRepo.FindByID(ctx, n.ConversationID); err == nil && conv != nil {
		channel = conv.Channel
	}
	payload := event.Merge(saved.FunnelDims(), map[string]any{"raw_message": n.RawText, "channel": channel}, n.EventExtra)
	if n.OriginMessageID != nil {
		payload["origin_message_id"] = n.OriginMessageID.String()
	}
	buyerID := n.BuyerID
	if err := s.events.Write(ctx, &event.Event{
		EventType: event.RequestCreated, EntityType: "buyer_request", EntityID: &saved.ID,
		ActorType: event.ActorBuyer, ActorID: &buyerID, Payload: payload,
	}); err != nil {
		s.logger.Error("writing REQUEST_CREATED event", "error", err)
	}
	return saved, true, nil
}

// CreateFromMessage creates a new EXTRACTING BuyerRequest from an inbound
// WhatsApp message (a thin wrapper over Create since Phase 4B). Extraction
// itself happens separately (see RunExtraction) so this can be called
// synchronously from the webhook handler (PRD 1 §12).
func (s *Service) CreateFromMessage(ctx context.Context, conversationID, buyerID, messageID uuid.UUID, rawText string) (*BuyerRequest, error) {
	created, _, err := s.Create(ctx, NewRequest{
		Origin: OriginMessage, ConversationID: conversationID, BuyerID: buyerID, OriginMessageID: &messageID, RawText: rawText,
	})
	return created, err
}

// ConfirmAs is Confirm with an explicit actor (the system auto-confirms
// listing-origin demand when DISCOVERY_ENQUIRY_CONFIRMATION=auto, Lock §O-1).
func (s *Service) ConfirmAs(ctx context.Context, requestID uuid.UUID, actor event.ActorType, reason string) (*BuyerRequest, error) {
	return s.confirm(ctx, requestID, nil, actor, reason)
}

// RunExtraction calls the AI service and persists the result. Designed to
// be safe to run in a background goroutine (see internal/jobs). Per
// PRD 1 §20: AI failure must never lose the buyer's request — on error,
// the request still transitions to NEEDS_REVIEW so an operator can
// manually complete it.
func (s *Service) RunExtraction(ctx context.Context, requestID uuid.UUID) {
	req, err := s.requests.FindByID(ctx, requestID)
	if err != nil {
		s.logger.Error("loading request for extraction", "request_id", requestID, "error", err)
		return
	}

	result, aiErr := s.aiProvider.Extract(ctx, ai.ExtractionRequest{
		Text:           req.RawMessage,
		ConversationID: req.ConversationID.String(),
	})

	if aiErr != nil {
		s.logger.Error("AI extraction failed, marking for manual review",
			"request_id", requestID, "error", aiErr)

		_, err := s.requests.UpdateExtraction(ctx, requestID, Fields{}, map[string]any{
			"error": aiErr.Error(),
		}, 0, StatusNeedsReview)
		if err != nil {
			s.logger.Error("persisting failed-extraction state", "request_id", requestID, "error", err)
		}

		if err := s.events.Write(ctx, &event.Event{
			EventType:  event.RequestExtracted,
			EntityType: "buyer_request",
			EntityID:   &requestID,
			ActorType:  event.ActorSystem,
			Payload:    map[string]any{"success": false, "error": aiErr.Error()},
		}); err != nil {
			s.logger.Error("writing failed REQUEST_EXTRACTED event", "error", err)
		}
		return
	}

	fields := Fields{
		Category:    result.Category,
		Product:     result.Product,
		Variant:     result.Variant,
		BudgetMin:   result.BudgetMin,
		BudgetMax:   result.BudgetMax,
		Location:    result.Location,
		Condition:   result.Condition,
		Quantity:    result.Quantity,
		Urgency:     result.Urgency,
		Preferences: result.Preferences,
	}

	extractedData := extractionResultToMap(result)

	updated, err := s.requests.UpdateExtraction(ctx, requestID, fields, extractedData, result.Confidence, StatusNeedsReview)
	if err != nil {
		s.logger.Error("persisting extraction result", "request_id", requestID, "error", err)
		return
	}

	if err := s.events.Write(ctx, &event.Event{
		EventType:  event.RequestExtracted,
		EntityType: "buyer_request",
		EntityID:   &requestID,
		ActorType:  event.ActorSystem,
		Payload: event.Merge(updated.FunnelDims(), map[string]any{
			"success":      true,
			"confidence":   result.Confidence,
			"completeness": string(updated.Completeness),
		}),
	}); err != nil {
		s.logger.Error("writing REQUEST_EXTRACTED event", "error", err)
	}

	s.logger.Info("extraction complete",
		"request_id", requestID,
		"completeness", updated.Completeness,
		"confidence", result.Confidence,
	)
}

// Correct applies an operator's field-level correction, recording a
// before/after audit entry and a REQUEST_CORRECTED event (PRD 1 §23-24).
// Only the fields present in `patch` (non-nil) are considered changed.
func (s *Service) Correct(ctx context.Context, requestID uuid.UUID, patch Fields, operatorID *uuid.UUID) (*BuyerRequest, error) {
	before, err := s.requests.FindByID(ctx, requestID)
	if err != nil {
		return nil, fmt.Errorf("loading request before correction: %w", err)
	}

	after, err := s.requests.UpdateFields(ctx, requestID, patch)
	if err != nil {
		return nil, fmt.Errorf("applying correction: %w", err)
	}

	prevValues, newValues := diffFields(before, patch)
	if len(prevValues) == 0 {
		// Nothing actually changed; still return the (unchanged) request
		// but skip writing a no-op audit/event record.
		return after, nil
	}

	if err := s.audits.Write(ctx, &audit.Entry{
		ActorType:     "OPERATOR",
		ActorID:       operatorID,
		Action:        "REQUEST_CORRECTED",
		EntityType:    "buyer_request",
		EntityID:      &requestID,
		PreviousValue: prevValues,
		NewValue:      newValues,
	}); err != nil {
		s.logger.Error("writing audit record for correction", "error", err)
	}

	if err := s.events.Write(ctx, &event.Event{
		EventType:  event.RequestCorrected,
		EntityType: "buyer_request",
		EntityID:   &requestID,
		ActorType:  event.ActorOperator,
		ActorID:    operatorID,
		Payload:    map[string]any{"previous": prevValues, "new": newValues},
	}); err != nil {
		s.logger.Error("writing REQUEST_CORRECTED event", "error", err)
	}

	return after, nil
}

// Confirm marks a request as CONFIRMED — the signal that Phase 2's vendor
// matching is allowed to consume it (PRD 1 §6/§49).
func (s *Service) Confirm(ctx context.Context, requestID uuid.UUID, operatorID *uuid.UUID) (*BuyerRequest, error) {
	return s.confirm(ctx, requestID, operatorID, event.ActorOperator, "")
}

func (s *Service) confirm(ctx context.Context, requestID uuid.UUID, operatorID *uuid.UUID, actor event.ActorType, reason string) (*BuyerRequest, error) {
	before, err := s.requests.FindByID(ctx, requestID)
	if err != nil {
		return nil, fmt.Errorf("loading request: %w", err)
	}
	confirmed, err := s.requests.Confirm(ctx, requestID)
	if err != nil {
		return nil, fmt.Errorf("confirming request: %w", err)
	}
	if before != nil && before.Status == StatusConfirmed {
		return confirmed, nil // idempotent re-confirm: no second funnel event
	}

	if err := s.events.Write(ctx, &event.Event{
		EventType:  event.RequestConfirmed,
		EntityType: "buyer_request",
		EntityID:   &requestID,
		ActorType:  actor,
		ActorID:    operatorID,
		Payload: event.Merge(confirmed.FunnelDims(), map[string]any{"completeness": string(confirmed.Completeness)},
			map[string]any{"confirmation": reasonOr(reason, "operator")}),
	}); err != nil {
		s.logger.Error("writing REQUEST_CONFIRMED event", "error", err)
	}

	return confirmed, nil
}

// Clarify sends a simple clarifying question back to the buyer when the
// request is too ambiguous to act on (PRD 1 §28). This is intentionally
// simple — no multi-turn AI orchestration, just a direct question chosen
// based on which important field is missing.
func (s *Service) Clarify(ctx context.Context, requestID uuid.UUID, question string) error {
	req, err := s.requests.FindByID(ctx, requestID)
	if err != nil {
		return fmt.Errorf("loading request for clarification: %w", err)
	}

	conv, err := s.convRepo.FindByID(ctx, req.ConversationID)
	if err != nil {
		return fmt.Errorf("loading conversation for clarification: %w", err)
	}

	if question == "" {
		question = defaultClarificationQuestion(req)
	}

	sendCtx := conversation.WithOutboundPurpose(ctx, conversation.OutboundPurpose{
		Kind: "clarification", EntityType: "buyer_request", EntityID: &requestID,
	})
	if err := s.messaging.SendText(sendCtx, conv.ExternalID, question); err != nil {
		return fmt.Errorf("sending clarification message: %w", err)
	}

	if err := s.events.Write(ctx, &event.Event{
		EventType:  event.ClarificationSent,
		EntityType: "buyer_request",
		EntityID:   &requestID,
		ActorType:  event.ActorOperator,
		Payload:    map[string]any{"question": question},
	}); err != nil {
		s.logger.Error("writing CLARIFICATION_SENT event", "error", err)
	}

	return nil
}

// --- helpers ---

func reasonOr(r, def string) string {
	if r == "" {
		return def
	}
	return r
}

func extractionResultToMap(r *ai.ExtractionResult) map[string]any {
	b, err := json.Marshal(r)
	if err != nil {
		return map[string]any{}
	}
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// diffFields compares only the fields present in patch against the
// current request, returning previous/new value maps suitable for an
// audit record. Fields absent from patch (nil) are not considered.
func diffFields(current *BuyerRequest, patch Fields) (previous, new map[string]any) {
	previous = map[string]any{}
	new = map[string]any{}

	if patch.Category != nil && notEqualStr(current.Category, patch.Category) {
		previous["category"] = derefStr(current.Category)
		new["category"] = derefStr(patch.Category)
	}
	if patch.Product != nil && notEqualStr(current.Product, patch.Product) {
		previous["product"] = derefStr(current.Product)
		new["product"] = derefStr(patch.Product)
	}
	if patch.Variant != nil && notEqualStr(current.Variant, patch.Variant) {
		previous["variant"] = derefStr(current.Variant)
		new["variant"] = derefStr(patch.Variant)
	}
	if patch.BudgetMin != nil && notEqualInt64(current.BudgetMin, patch.BudgetMin) {
		previous["budget_min"] = derefInt64(current.BudgetMin)
		new["budget_min"] = derefInt64(patch.BudgetMin)
	}
	if patch.BudgetMax != nil && notEqualInt64(current.BudgetMax, patch.BudgetMax) {
		previous["budget_max"] = derefInt64(current.BudgetMax)
		new["budget_max"] = derefInt64(patch.BudgetMax)
	}
	if patch.Location != nil && notEqualStr(current.Location, patch.Location) {
		previous["location"] = derefStr(current.Location)
		new["location"] = derefStr(patch.Location)
	}
	if patch.Condition != nil && notEqualStr(current.Condition, patch.Condition) {
		previous["condition"] = derefStr(current.Condition)
		new["condition"] = derefStr(patch.Condition)
	}
	if patch.Quantity != nil && notEqualInt(current.Quantity, patch.Quantity) {
		previous["quantity"] = derefInt(current.Quantity)
		new["quantity"] = derefInt(patch.Quantity)
	}
	if patch.Urgency != nil && notEqualStr(current.Urgency, patch.Urgency) {
		previous["urgency"] = derefStr(current.Urgency)
		new["urgency"] = derefStr(patch.Urgency)
	}

	return previous, new
}

func derefStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func derefInt64(i *int64) any {
	if i == nil {
		return nil
	}
	return *i
}

func derefInt(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

func notEqualStr(a, b *string) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return *a != *b
}

func notEqualInt64(a, b *int64) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return *a != *b
}

func notEqualInt(a, b *int) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return *a != *b
}

func defaultClarificationQuestion(req *BuyerRequest) string {
	switch {
	case req.Product == nil || *req.Product == "":
		return "Sorry, could you tell me a bit more about what you're looking for?"
	case req.BudgetMin == nil && req.BudgetMax == nil:
		return "Got it — what's your budget for that?"
	case req.Location == nil || *req.Location == "":
		return "Thanks — which city or area should I search in?"
	default:
		return "Could you share a bit more detail so I can find the best options?"
	}
}
