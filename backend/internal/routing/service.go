package routing

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"regexp"
	"strings"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/matching"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/vendor"
)

// MessagingProvider is the minimal outbound surface this service needs —
// defined locally so the routing domain stays independent of any specific
// channel implementation (mirrors the pattern in internal/request/service.go).
type MessagingProvider interface {
	SendText(ctx context.Context, to, body string) error
}

// Service orchestrates the Phase 2 vendor/supply engine: generating
// match candidates for a confirmed BuyerRequest, creating and sending
// VendorRequests once an operator approves a shortlist, and processing
// inbound vendor messages (supply submissions and VendorRequest replies).
type Service struct {
	vendors    vendor.Repository
	supplies   supply.Repository
	routing    Repository
	requests   request.Repository
	aiProvider ai.Provider
	events     event.Writer
	audits     audit.Writer
	messaging  MessagingProvider
	logger     *slog.Logger

	// listener is notified after a VendorResponse is created, corrected or
	// re-associated (Phase 3.1). Set via SetResponseListener; nil means
	// nobody is listening (Phase 2 behaviour).
	listener ResponseListener

	// intake groups vendor supply content (text, photos, captions) into
	// SupplySignals with source provenance (Phase 4A.2). nil = the Phase 2
	// text-only path (processAsSupplySubmission).
	intake SupplyIntake
}

// SupplyIntake receives vendor messages classified as supply content
// (Phase 4A.2). Implemented by supply.Intake.
type SupplyIntake interface {
	SubmitText(ctx context.Context, vendorID uuid.UUID, ref supply.Inbound, text string) error
	SubmitMedia(ctx context.Context, vendorID uuid.UUID, ref supply.Inbound, caption, reason string, ext *ai.SupplyExtractionResult) error
}

// SetSupplyIntake wires Phase 4A.2 supply grouping (composition root).
func (s *Service) SetSupplyIntake(i SupplyIntake) { s.intake = i }

// ResponseListener is notified whenever a VendorResponse that is attached
// to a VendorRequest is recorded or changed. It exists so the buyer-facing
// offer loop (internal/marketplace) can react to vendor replies without the
// routing package importing the offer package (which already imports
// routing). Implementations must be safe to call repeatedly for the same
// response (idempotent).
type ResponseListener interface {
	VendorResponseChanged(ctx context.Context, resp *VendorResponse)
}

// SetResponseListener wires the post-response hook. Called once from the
// composition root.
func (s *Service) SetResponseListener(l ResponseListener) { s.listener = l }

func (s *Service) notifyResponseChanged(ctx context.Context, resp *VendorResponse) {
	if s.listener == nil || resp == nil || resp.VendorRequestID == nil {
		return
	}
	s.listener.VendorResponseChanged(ctx, resp)
}

func NewService(
	vendors vendor.Repository,
	supplies supply.Repository,
	routingRepo Repository,
	requests request.Repository,
	aiProvider ai.Provider,
	events event.Writer,
	audits audit.Writer,
	messaging MessagingProvider,
	logger *slog.Logger,
) *Service {
	return &Service{
		vendors: vendors, supplies: supplies, routing: routingRepo, requests: requests,
		aiProvider: aiProvider, events: events, audits: audits, messaging: messaging, logger: logger,
	}
}

// SuggestVendors runs the deterministic matching engine against a
// confirmed BuyerRequest (PRD 2 §16-28). Returns ranked candidates for
// operator review — this never sends anything to vendors by itself.
func (s *Service) SuggestVendors(ctx context.Context, buyerRequestID uuid.UUID) ([]matching.Candidate, *request.BuyerRequest, error) {
	req, err := s.requests.FindByID(ctx, buyerRequestID)
	if err != nil {
		return nil, nil, fmt.Errorf("loading buyer request: %w", err)
	}
	// Phase 3.1 §3A-2: matching is marketplace execution — an unconfirmed
	// request (still EXTRACTING / NEEDS_REVIEW) never enters it.
	if err := req.EnsureConfirmed(); err != nil {
		return nil, req, err
	}

	category := ""
	if req.Category != nil {
		category = *req.Category
	}
	_ = category // category is still used by the matching engine itself for scoring/eligibility

	// Load all ACTIVE vendors rather than pre-filtering by exact category
	// string in SQL. Categories aren't always exact matches (PRD 2 §7 lists
	// "Electronics" and "Phones" as siblings even though Phase 1's broad
	// extraction only ever produces "electronics") — the matching engine's
	// categoryFamilies compatibility logic is the single source of truth
	// for eligibility, and a SQL-level pre-filter would silently exclude
	// vendors before that logic ever runs. Vendor counts are pilot-scale
	// (PRD 2 §61: "hundreds of vendors"), so loading them all is cheap.
	active := vendor.StatusActive
	candidateVendors, err := s.vendors.List(ctx, &active, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("loading candidate vendors: %w", err)
	}

	supplyByVendor := map[string][]*supply.Signal{}
	for _, v := range candidateVendors {
		signals, err := s.supplies.FreshByVendor(ctx, v.ID)
		if err != nil {
			s.logger.Error("loading vendor supply", "vendor_id", v.ID, "error", err)
			continue
		}
		supplyByVendor[v.ID.String()] = signals
	}

	candidates := matching.Match(
		req, candidateVendors, supplyByVendor,
		matching.DefaultWeights(), matching.DefaultMinRelevanceScore, matching.DefaultMaxCandidates,
	)

	if err := s.events.Write(ctx, &event.Event{
		EventType:  event.VendorsSuggested,
		EntityType: "buyer_request",
		EntityID:   &buyerRequestID,
		ActorType:  event.ActorSystem,
		Payload:    map[string]any{"candidate_count": len(candidates)},
	}); err != nil {
		s.logger.Error("writing VENDORS_SUGGESTED event", "error", err)
	}

	return candidates, req, nil
}

// RouteToVendors is the operator-approval step (PRD 2 §29-33): creates a
// VendorRequest per approved vendor and sends the WhatsApp message. Never
// called automatically — matching only ever suggests.
func (s *Service) RouteToVendors(ctx context.Context, req *request.BuyerRequest, vendorIDs []uuid.UUID, scores map[uuid.UUID]int, breakdowns map[uuid.UUID]map[string]any) ([]*VendorRequest, error) {
	// Phase 3.1 §3A-2: never route an unconfirmed request to vendors.
	if err := req.EnsureConfirmed(); err != nil {
		return nil, err
	}

	var created []*VendorRequest

	for _, vendorID := range vendorIDs {
		v, err := s.vendors.FindByID(ctx, vendorID)
		if err != nil || v == nil {
			s.logger.Error("loading vendor for routing", "vendor_id", vendorID, "error", err)
			continue
		}

		var scorePtr *int
		if sc, ok := scores[vendorID]; ok {
			scorePtr = &sc
		}
		breakdown := breakdowns[vendorID]

		vr, err := s.createVendorRequestWithReference(ctx, &VendorRequest{
			BuyerRequestID: req.ID,
			VendorID:       vendorID,
			MatchScore:     scorePtr,
			MatchBreakdown: breakdown,
		})
		if err != nil {
			// ErrDuplicateVendorRequest: PRD 2 §47 unique pair — this
			// vendor was already asked about this request.
			s.logger.Error("creating vendor request", "vendor_id", vendorID, "error", err)
			continue
		}
		ref := vr.PublicReference

		message := buildVendorRequestMessage(ref, req)
		vrID := vr.ID
		sendCtx := conversation.WithOutboundPurpose(ctx, conversation.OutboundPurpose{
			Kind: "vendor_request", EntityType: "vendor_request", EntityID: &vrID,
			Extra: map[string]any{"buyer_request_id": req.ID.String(), "vendor_id": vendorID.String(), "reference": ref},
		})
		if err := s.messaging.SendText(sendCtx, v.WhatsAppExternalID, message); err != nil {
			s.logger.Error("sending vendor request via whatsapp", "vendor_id", vendorID, "error", err)
			// Phase 3.1 §3A-4: a rejected send is a distinct, visible
			// state (SEND_FAILED), never an "awaiting reply" request.
			if mErr := s.routing.MarkSendFailed(ctx, vr.ID, err.Error()); mErr != nil {
				s.logger.Error("marking vendor request send-failed", "error", mErr)
			}
			vr.Status = VendorRequestSendFailed
			errStr := err.Error()
			vr.SendError = &errStr
			if wErr := s.events.Write(ctx, &event.Event{
				EventType: event.VendorRequestSendFailed, EntityType: "vendor_request", EntityID: &vr.ID,
				ActorType: event.ActorSystem,
				Payload:   map[string]any{"buyer_request_id": req.ID.String(), "vendor_id": vendorID.String(), "reference": ref, "error": err.Error()},
			}); wErr != nil {
				s.logger.Error("writing VENDOR_REQUEST_SEND_FAILED event", "error", wErr)
			}
		} else {
			if err := s.routing.MarkSent(ctx, vr.ID); err != nil {
				s.logger.Error("marking vendor request sent", "error", err)
			}
			vr.Status = VendorRequestSent
			_ = s.vendors.RecordRequestSent(ctx, vendorID)
		}

		// Funnel: one VENDORS_ROUTED per VendorRequest created; outcome
		// records whether WhatsApp accepted the send ("sent") or rejected it
		// ("send_failed"). Actual delivery/read arrive later as
		// MESSAGE_DELIVERED / MESSAGE_READ on the vendor_request entity.
		outcome := "sent"
		if vr.Status == VendorRequestSendFailed {
			outcome = "send_failed"
		}
		if err := s.events.Write(ctx, &event.Event{
			EventType:  event.VendorsRouted,
			EntityType: "vendor_request",
			EntityID:   &vr.ID,
			ActorType:  event.ActorOperator,
			Payload: event.Merge(req.FunnelDims(), map[string]any{
				"vendor_request_id": vr.ID.String(), "vendor_id": vendorID.String(), "reference": ref,
				"match_score": scorePtr, "channel": "whatsapp", "outcome": outcome,
			}),
		}); err != nil {
			s.logger.Error("writing VENDORS_ROUTED event", "error", err)
		}

		created = append(created, vr)
	}

	if err := s.events.Write(ctx, &event.Event{
		EventType:  event.VendorShortlistModified,
		EntityType: "buyer_request",
		EntityID:   &req.ID,
		ActorType:  event.ActorOperator,
		Payload:    map[string]any{"approved_vendor_count": len(created)},
	}); err != nil {
		s.logger.Error("writing VENDOR_SHORTLIST_MODIFIED event", "error", err)
	}

	return created, nil
}

// ProcessVendorMessage is the entry point for any inbound WhatsApp message
// from a known vendor. Per PRD 2 §39-40: prefer an active (SENT, not yet
// RESPONDED) VendorRequest; if none exists, treat the message as a supply
// submission instead.
// mmReferencePattern extracts an explicit "MM-1234"-style reference from
// a vendor's free-text reply (PRD 2.1 §4 priority 1).
var mmReferencePattern = regexp.MustCompile(`(?i)\bMM-(\d+)\b`)

// ProcessVendorMessage is the entry point for any inbound WhatsApp message
// from a known vendor. Implements the PRD 2.1 §4 association priority:
//  1. Explicit "MM-XXXX" reference in the message, if it exists and
//     belongs to this vendor.
//  2. Exactly one active (PENDING/SENT) VendorRequest for this vendor.
//  3. Multiple active requests with no reference -> do NOT guess; persist
//     the response unmatched (vendor_request_id = NULL) for operator
//     resolution.
//  4. No active requests at all -> treat as a supply submission instead
//     (existing PRD 2 §39-40 behavior, unchanged).
//
// Returns the persisted VendorResponse (nil when the message was treated as
// a supply submission or could not be persisted). After a response attached
// to a VendorRequest is persisted, the ResponseListener is notified so the
// buyer-facing offer loop can run.
func (s *Service) ProcessVendorMessage(ctx context.Context, v *vendor.Vendor, rawText string) *VendorResponse {
	return s.ProcessVendorMessageFrom(ctx, v, supply.Inbound{}, rawText)
}

// ProcessVendorMessageFrom is ProcessVendorMessage with the inbound
// message's identity (Phase 4A.2), so a text that becomes supply records
// its source message. VendorRequest handling is unchanged.
func (s *Service) ProcessVendorMessageFrom(ctx context.Context, v *vendor.Vendor, ref supply.Inbound, rawText string) *VendorResponse {
	_ = s.vendors.RecordMessageActivity(ctx, v.ID)

	active, err := s.routing.ListActiveByVendor(ctx, v.ID)
	if err != nil {
		s.logger.Error("listing active vendor requests", "vendor_id", v.ID, "error", err)
		return nil
	}

	// Priority 1: explicit MM-XXXX reference, checked regardless of how
	// many active requests exist — an explicit reference is always safe
	// to honor if it resolves and belongs to this vendor.
	if m := mmReferencePattern.FindStringSubmatch(rawText); m != nil {
		ref := "MM-" + m[1]
		referenced, err := s.routing.FindVendorRequestByPublicReference(ctx, ref)
		if err != nil {
			s.logger.Error("looking up vendor request by reference", "reference", ref, "error", err)
		}
		switch {
		case referenced == nil:
			// Reference doesn't exist — do not guess (PRD 2.1 §4).
			s.logger.Info("vendor cited an unknown reference, persisting unmatched", "vendor_id", v.ID, "reference", ref)
			return s.processAsVendorResponse(ctx, v, nil, rawText)
		case referenced.VendorID != v.ID:
			// Reference exists but belongs to a different vendor — never
			// cross-attach (PRD 2.1 §4 test: "request belonging to
			// another vendor").
			s.logger.Info("vendor cited a reference belonging to another vendor, persisting unmatched",
				"vendor_id", v.ID, "reference", ref, "actual_owner", referenced.VendorID)
			return s.processAsVendorResponse(ctx, v, nil, rawText)
		default:
			resp := s.processAsVendorResponse(ctx, v, referenced, rawText)
			s.notifyResponseChanged(ctx, resp)
			return resp
		}
	}

	switch len(active) {
	case 0:
		// No active request at all -> supply submission (PRD 2 §39-40).
		if s.intake != nil && ref.MessageID != uuid.Nil {
			if err := s.intake.SubmitText(ctx, v.ID, ref, rawText); err != nil {
				s.logger.Error("supply intake failed for vendor text", "vendor_id", v.ID, "message_id", ref.MessageID, "error", err)
			}
			return nil
		}
		s.processAsSupplySubmission(ctx, v, rawText)
		return nil
	case 1:
		// Priority 2/3: exactly one active request — safe to use.
		resp := s.processAsVendorResponse(ctx, v, active[0], rawText)
		s.notifyResponseChanged(ctx, resp)
		return resp
	default:
		// Priority 4: multiple active requests, no reference to
		// disambiguate. PRD 2.1 §4 is explicit: DO NOT AUTO-ASSOCIATE.
		s.logger.Info("vendor has multiple active requests and no reference, persisting unmatched",
			"vendor_id", v.ID, "active_count", len(active))
		return s.processAsVendorResponse(ctx, v, nil, rawText)
	}
}

// Media supply classification reasons (Phase 4A.2). Stable strings: they
// are recorded on the candidate and logged.
const (
	MediaSupplyNoActiveRequest  = "no_active_vendor_request"
	MediaSupplyUnrelatedCaption = "caption_has_product_and_price_unrelated_to_active_request"
	mediaNotSupplyReference     = "caption_cites_vendor_request_reference"
	mediaNotSupplyNoCaption     = "uncaptioned_media_during_active_vendor_request"
	mediaNotSupplyReply         = "caption_reads_as_a_reply"
	mediaNotSupplyIncomplete    = "caption_lacks_product_or_price"
	mediaNotSupplySameProduct   = "caption_matches_an_active_request_product"
	mediaNotSupplyNoExtraction  = "caption_extraction_unavailable"
	mediaNotSupplyLookupFailed  = "vendor_request_lookup_failed"
)

// replyOpeners: captions starting with these read as an answer to Majo's
// question, not as new supply. Deliberately small and explicit.
var replyOpeners = regexp.MustCompile(`(?i)^\s*(yes|yeah|yea|yep|yup|no|nope|ok|okay|sure|sorry|available|not available|i get|i have|we get|we have|e dey|dey|sold|na this|this one)\b`)

// ClassifyVendorMedia decides deterministically whether a vendor photo/video
// (with its caption) is supply content. Phase 4A.2 rule (Architecture Lock
// V2 §M 4A step 3, implemented conservatively):
//
//   - caption cites an MM- reference -> not supply (it is about a request);
//   - no active VendorRequest -> supply;
//   - active VendorRequest(s) -> supply ONLY if the caption names a product
//     AND a price (existing heuristic extractor), does not open like a reply
//     ("yes", "available", "I get" ...), and its product does not overlap
//     any active request's product. Anything else keeps the existing
//     behaviour: the media is stored but the message is not processed.
//
// ext is the caption's extraction when one was made (reused by intake).
func (s *Service) ClassifyVendorMedia(ctx context.Context, v *vendor.Vendor, caption string) (bool, string, *ai.SupplyExtractionResult) {
	if mmReferencePattern.MatchString(caption) {
		return false, mediaNotSupplyReference, nil
	}
	active, err := s.routing.ListActiveByVendor(ctx, v.ID)
	if err != nil {
		s.logger.Error("listing active vendor requests", "vendor_id", v.ID, "error", err)
		return false, mediaNotSupplyLookupFailed, nil
	}
	if len(active) == 0 {
		return true, MediaSupplyNoActiveRequest, nil
	}
	if strings.TrimSpace(caption) == "" {
		return false, mediaNotSupplyNoCaption, nil
	}
	if replyOpeners.MatchString(caption) {
		return false, mediaNotSupplyReply, nil
	}
	ext, err := s.aiProvider.ExtractSupply(ctx, caption)
	if err != nil {
		return false, mediaNotSupplyNoExtraction, nil
	}
	if ext.Product == nil || *ext.Product == "" || ext.Price == nil || *ext.Price <= 0 {
		return false, mediaNotSupplyIncomplete, ext
	}
	captionProduct := normalizeForCompare(*ext.Product)
	for _, vr := range active {
		req, err := s.requests.FindByID(ctx, vr.BuyerRequestID)
		if err != nil || req == nil {
			return false, mediaNotSupplyLookupFailed, ext
		}
		if req.Product != nil {
			rp := normalizeForCompare(*req.Product)
			if rp != "" && (strings.Contains(rp, captionProduct) || strings.Contains(captionProduct, rp)) {
				return false, mediaNotSupplySameProduct, ext
			}
		}
	}
	return true, MediaSupplyUnrelatedCaption, ext
}

func normalizeForCompare(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), ""))
}

// ProcessVendorMedia handles an inbound photo/video from an active vendor
// (Phase 4A.2): classify it, and hand supply content to grouping. Media that
// is not supply keeps the pre-4A.2 behaviour (stored, not processed).
func (s *Service) ProcessVendorMedia(ctx context.Context, v *vendor.Vendor, ref supply.Inbound, kind, caption string) {
	_ = s.vendors.RecordMessageActivity(ctx, v.ID)
	if s.intake == nil || !supply.IsSupplyMediaKind(kind) {
		return
	}
	ok, reason, ext := s.ClassifyVendorMedia(ctx, v, caption)
	if !ok {
		s.logger.Info("vendor media not treated as supply", "vendor_id", v.ID, "message_id", ref.MessageID, "reason", reason)
		return
	}
	if err := s.intake.SubmitMedia(ctx, v.ID, ref, caption, reason, ext); err != nil {
		s.logger.Error("supply intake failed for vendor media", "vendor_id", v.ID, "message_id", ref.MessageID, "error", err)
		return
	}
	s.logger.Info("vendor media accepted as supply content", "vendor_id", v.ID, "message_id", ref.MessageID, "reason", reason)
}

func (s *Service) processAsSupplySubmission(ctx context.Context, v *vendor.Vendor, rawText string) {
	result, err := s.aiProvider.ExtractSupply(ctx, rawText)
	extractedData := map[string]any{}
	availability := supply.AvailabilityAvailable
	var category, product, variant, condition, location *string
	var price *int64
	var quantity *int
	confidence := 0.0

	if err != nil {
		s.logger.Error("supply extraction failed, persisting raw message for review", "vendor_id", v.ID, "error", err)
		extractedData["error"] = err.Error()
	} else {
		category, product, variant, condition, location = result.Category, result.Product, result.Variant, result.Condition, result.Location
		price, quantity, confidence = result.Price, result.Quantity, result.Confidence
		availability = supply.Availability(result.AvailabilityStatus)
		extractedData = map[string]any{
			"category": result.Category, "product": result.Product, "variant": result.Variant,
			"condition": result.Condition, "price": result.Price, "quantity": result.Quantity,
			"location": result.Location, "availability_status": result.AvailabilityStatus,
		}
	}

	signal, err := s.supplies.Create(ctx, &supply.Signal{
		VendorID: v.ID, Category: category, Product: product, Variant: variant, Condition: condition,
		Price: price, Quantity: quantity, Location: location, AvailabilityStatus: availability,
		RawMessage: rawText, ExtractedData: extractedData, Confidence: confidence,
	})
	if err != nil {
		s.logger.Error("persisting supply signal", "vendor_id", v.ID, "error", err)
		return
	}

	_ = s.vendors.RecordSupplySubmission(ctx, v.ID)

	if err := s.events.Write(ctx, &event.Event{
		EventType:  event.SupplySignalCreated,
		EntityType: "supply_signal",
		EntityID:   &signal.ID,
		ActorType:  event.ActorVendor,
		ActorID:    &v.ID,
		Payload: event.Merge(map[string]any{
			"raw_message": rawText, "vendor_id": v.ID.String(), "category": category, "product": product,
			"variant": variant, "price": price, "location": location, "availability": string(availability),
			"confidence": confidence, "channel": "whatsapp",
		}),
	}); err != nil {
		s.logger.Error("writing SUPPLY_SIGNAL_CREATED event", "error", err)
	}

	s.logger.Info("supply signal created", "vendor_id", v.ID, "signal_id", signal.ID, "product", derefStr(product))
}

// processAsVendorResponse persists a vendor's reply. vr may be nil — that
// means the response could not be safely associated with any specific
// VendorRequest (PRD 2.1 §4/§13); it is still persisted, attributed to
// the vendor, with vendor_request_id left NULL for later operator
// association (see AssociateResponse).
//
// Extraction failure never produces UNAVAILABLE (PRD 2.1 §8): on error,
// the response is recorded UNCLASSIFIED + NEEDS_REVIEW, exactly like a
// heuristically-ambiguous message, and the raw message is always
// preserved either way.
func (s *Service) processAsVendorResponse(ctx context.Context, v *vendor.Vendor, vr *VendorRequest, rawText string) *VendorResponse {
	result, err := s.aiProvider.ExtractVendorResponse(ctx, rawText)

	status := ResponseUnclassified
	reviewState := ReviewNeedsReview
	extractionMethod := ExtractionHeuristic
	var product, variant, condition, location, notes *string
	var price *int64
	var quantity *int
	confidence := 0.0
	extractedData := map[string]any{}

	if err != nil {
		s.logger.Error("vendor response extraction failed, marking UNCLASSIFIED (never UNAVAILABLE)",
			"vendor_id", v.ID, "error", err)
		extractedData["error"] = err.Error()
	} else {
		status = ResponseStatus(result.Status)
		if result.ReviewState != "" {
			reviewState = ReviewState(result.ReviewState)
		}
		if result.ExtractionMethod != "" {
			extractionMethod = ExtractionMethod(result.ExtractionMethod)
		}
		product, variant, condition, location, notes = result.Product, result.Variant, result.Condition, result.Location, result.Notes
		price, quantity, confidence = result.Price, result.Quantity, result.Confidence
		extractedData = map[string]any{
			"status": result.Status, "review_state": result.ReviewState, "extraction_method": result.ExtractionMethod,
			"product": result.Product, "variant": result.Variant,
			"condition": result.Condition, "price": result.Price, "quantity": result.Quantity,
			"location": result.Location, "notes": result.Notes,
		}
	}

	var vendorRequestID *uuid.UUID
	if vr != nil {
		vendorRequestID = &vr.ID
	} else {
		// Unmatched (PRD 2.1 §4): even if the business classification
		// itself was confident, an unassociated response always needs
		// operator attention to resolve *which* request it belongs to.
		// Association ambiguity and classification ambiguity are
		// separate reasons for review, and either one is sufficient.
		reviewState = ReviewNeedsReview
	}

	resp, err := s.routing.CreateVendorResponse(ctx, &VendorResponse{
		VendorRequestID: vendorRequestID, VendorID: v.ID, Status: status, ReviewState: reviewState,
		Product: product, Variant: variant, Condition: condition,
		Price: price, Quantity: quantity, Location: location, Notes: notes,
		RawMessage: rawText, ExtractedData: extractedData, Confidence: confidence,
		ExtractionMethod: extractionMethod,
	})
	if err != nil {
		s.logger.Error("persisting vendor response", "vendor_id", v.ID, "error", err)
		return nil
	}

	_ = s.vendors.RecordResponseActivity(ctx, v.ID)

	entityID := resp.ID
	payload := event.Merge(map[string]any{
		"status": string(status), "review_state": string(reviewState), "raw_message": rawText, "matched": vr != nil,
		"vendor_id": v.ID.String(), "vendor_response_id": resp.ID.String(), "price": price,
		"extraction_method": string(extractionMethod), "channel": "whatsapp",
	})
	if vr != nil {
		payload["vendor_request_id"] = vr.ID.String()
		payload["buyer_request_id"] = vr.BuyerRequestID.String()
		if vr.SentAt != nil {
			payload["seconds_since_request_sent"] = int64(resp.CreatedAt.Sub(*vr.SentAt).Seconds())
		}
	}
	vendorID := v.ID
	if err := s.events.Write(ctx, &event.Event{
		EventType: event.VendorResponseReceived, EntityType: "vendor_response", EntityID: &entityID,
		ActorType: event.ActorVendor, ActorID: &vendorID, Payload: payload,
	}); err != nil {
		s.logger.Error("writing VENDOR_RESPONSE_RECEIVED event", "error", err)
	}

	s.logger.Info("vendor response recorded",
		"vendor_id", v.ID, "response_id", resp.ID, "status", status, "review_state", reviewState, "matched", vr != nil)
	return resp
}

// CorrectResponse applies an operator correction to a vendor response's
// structured fields (PRD 2.1 §12). Loads the current state first so the
// audit record carries real before/after values — the previous
// implementation wrote an audit action with no diff content at all,
// which the PRD 2.1 audit flagged as a compliance gap.
func (s *Service) CorrectResponse(ctx context.Context, responseID uuid.UUID, fields ResponseFields, operatorID *uuid.UUID) (*VendorResponse, error) {
	before, err := s.routing.FindResponseByID(ctx, responseID)
	if err != nil {
		return nil, fmt.Errorf("loading response before correction: %w", err)
	}
	if before == nil {
		return nil, fmt.Errorf("vendor response %s not found", responseID)
	}

	// Correcting a response is itself a human review action, per PRD 2.1
	// §1/§12: it must move ReviewState to CORRECTED regardless of what
	// the caller passed (an operator correction always means CORRECTED).
	corrected := ReviewCorrected
	fields.ReviewState = &corrected
	manual := ExtractionManual
	// Only override extraction_method to "manual" if the operator is
	// actually changing a structured field (not just, hypothetically,
	// re-saving with no changes) — computed below alongside the diff.

	after, err := s.routing.UpdateVendorResponse(ctx, responseID, fields)
	if err != nil {
		return nil, fmt.Errorf("applying correction: %w", err)
	}

	prevValues, newValues := diffResponseFields(before, fields)

	if len(prevValues) > 0 {
		// A structured field actually changed — record that this field
		// data is now operator-provided, not purely automated (PRD 2.1 §8).
		fields.ExtractionMethod = &manual
		if reAfter, err := s.routing.UpdateVendorResponse(ctx, responseID, ResponseFields{ExtractionMethod: &manual}); err == nil {
			after = reAfter
		}

		if err := s.audits.Write(ctx, &audit.Entry{
			ActorType: "OPERATOR", ActorID: operatorID, Action: "VENDOR_RESPONSE_CORRECTED",
			EntityType: "vendor_response", EntityID: &responseID,
			PreviousValue: prevValues, NewValue: newValues,
		}); err != nil {
			// PRD 2.1 §6: "the API must not return apparent success while
			// silently failing to write the audit." A correction without
			// an audit trail is not a safe correction — surface the error
			// to the caller instead of swallowing it.
			return nil, fmt.Errorf("correction applied but audit write failed: %w", err)
		}
	}

	if err := s.events.Write(ctx, &event.Event{
		EventType: event.VendorResponseCorrected, EntityType: "vendor_response", EntityID: &responseID,
		ActorType: event.ActorOperator, ActorID: operatorID,
		Payload: map[string]any{"previous": prevValues, "new": newValues},
	}); err != nil {
		s.logger.Error("writing VENDOR_RESPONSE_CORRECTED event", "error", err)
	}

	// A correction can make a response newly eligible for an Offer (e.g.
	// UNCLASSIFIED -> AVAILABLE) or ineligible (-> UNAVAILABLE).
	s.notifyResponseChanged(ctx, after)
	return after, nil
}

// AssociateResponse implements the operator's manual-association action
// (PRD 2.1 §5): attaching an unmatched (or wrongly-matched) response to
// the correct VendorRequest. Writes an audit record with the previous
// and new vendor_request_id.
func (s *Service) AssociateResponse(ctx context.Context, responseID, newVendorRequestID uuid.UUID, operatorID *uuid.UUID) (*VendorResponse, error) {
	newVR, err := s.routing.FindVendorRequestByID(ctx, newVendorRequestID)
	if err != nil {
		return nil, fmt.Errorf("loading target vendor request: %w", err)
	}
	if newVR == nil {
		return nil, fmt.Errorf("vendor request %s: %w", newVendorRequestID, ErrNotFound)
	}

	// Phase 3.1 §3A-3: vendor identity is authoritative. A response may
	// only ever be attached to a VendorRequest that was sent to the SAME
	// vendor. There is no privileged override (no operator auth exists to
	// make one safe); a genuinely mis-attributed vendor must be fixed at
	// the vendor-identity level, not by re-pointing the response.
	resp, err := s.routing.FindResponseByID(ctx, responseID)
	if err != nil {
		return nil, fmt.Errorf("loading vendor response: %w", err)
	}
	if resp == nil {
		return nil, fmt.Errorf("vendor response %s: %w", responseID, ErrNotFound)
	}
	if resp.VendorID != newVR.VendorID {
		if wErr := s.events.Write(ctx, &event.Event{
			EventType: event.VendorResponseAssociationRejected, EntityType: "vendor_response", EntityID: &responseID,
			ActorType: event.ActorOperator, ActorID: operatorID,
			Payload: map[string]any{
				"response_vendor_id": resp.VendorID.String(), "target_vendor_request_id": newVendorRequestID.String(),
				"target_vendor_id": newVR.VendorID.String(), "reason": "cross_vendor",
			},
		}); wErr != nil {
			s.logger.Error("writing association-rejected event", "error", wErr)
		}
		return nil, ErrCrossVendorAssociation
	}

	previous, updated, err := s.routing.UpdateAssociation(ctx, responseID, newVendorRequestID)
	if err != nil {
		return nil, fmt.Errorf("updating association: %w", err)
	}

	previousValue := map[string]any{"vendor_request_id": nil}
	if previous != nil {
		previousValue["vendor_request_id"] = previous.String()
	}
	newValue := map[string]any{"vendor_request_id": newVendorRequestID.String()}

	if err := s.audits.Write(ctx, &audit.Entry{
		ActorType: "OPERATOR", ActorID: operatorID, Action: "VENDOR_RESPONSE_ASSOCIATION_CORRECTED",
		EntityType: "vendor_response", EntityID: &responseID,
		PreviousValue: previousValue, NewValue: newValue,
	}); err != nil {
		return nil, fmt.Errorf("association applied but audit write failed: %w", err)
	}

	if err := s.events.Write(ctx, &event.Event{
		EventType: event.VendorResponseAssociationCorrected, EntityType: "vendor_response", EntityID: &responseID,
		ActorType: event.ActorOperator, ActorID: operatorID,
		Payload: map[string]any{"previous": previousValue, "new": newValue},
	}); err != nil {
		s.logger.Error("writing VENDOR_RESPONSE_ASSOCIATION_CORRECTED event", "error", err)
	}

	s.notifyResponseChanged(ctx, updated)
	return updated, nil
}

// diffResponseFields compares only the fields present in patch against
// the current response, returning previous/new value maps for an audit
// record. Mirrors internal/request/service.go's diffFields — deliberately
// not generalized into a shared helper across packages, since the field
// sets differ and the extra abstraction wouldn't reduce real duplication
// (each still needs its own per-field type handling).
func diffResponseFields(current *VendorResponse, patch ResponseFields) (previous, new map[string]any) {
	previous = map[string]any{}
	new = map[string]any{}

	if patch.Status != nil && (current.Status != *patch.Status) {
		previous["status"] = string(current.Status)
		new["status"] = string(*patch.Status)
	}
	if patch.Product != nil && notEqualStrP(current.Product, patch.Product) {
		previous["product"] = derefStrP(current.Product)
		new["product"] = derefStrP(patch.Product)
	}
	if patch.Variant != nil && notEqualStrP(current.Variant, patch.Variant) {
		previous["variant"] = derefStrP(current.Variant)
		new["variant"] = derefStrP(patch.Variant)
	}
	if patch.Condition != nil && notEqualStrP(current.Condition, patch.Condition) {
		previous["condition"] = derefStrP(current.Condition)
		new["condition"] = derefStrP(patch.Condition)
	}
	if patch.Price != nil && notEqualInt64P(current.Price, patch.Price) {
		previous["price"] = derefInt64P(current.Price)
		new["price"] = derefInt64P(patch.Price)
	}
	if patch.Quantity != nil && notEqualIntP(current.Quantity, patch.Quantity) {
		previous["quantity"] = derefIntP(current.Quantity)
		new["quantity"] = derefIntP(patch.Quantity)
	}
	if patch.Location != nil && notEqualStrP(current.Location, patch.Location) {
		previous["location"] = derefStrP(current.Location)
		new["location"] = derefStrP(patch.Location)
	}
	if patch.Notes != nil && notEqualStrP(current.Notes, patch.Notes) {
		previous["notes"] = derefStrP(current.Notes)
		new["notes"] = derefStrP(patch.Notes)
	}

	return previous, new
}

func notEqualStrP(a, b *string) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return *a != *b
}

func notEqualInt64P(a, b *int64) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return *a != *b
}

func notEqualIntP(a, b *int) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return *a != *b
}

func derefStrP(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

func derefInt64P(i *int64) any {
	if i == nil {
		return nil
	}
	return *i
}

func derefIntP(i *int) any {
	if i == nil {
		return nil
	}
	return *i
}

// --- helpers ---

// maxReferenceAttempts bounds retries when a generated public reference
// collides with an existing one.
const maxReferenceAttempts = 8

// generateReference returns a random human-readable public reference.
//
// Phase 3.1 §3A-5: the original 4-digit space (MM-1000..MM-9999, 9,000
// values, globally unique and never reused) made collisions likely within
// a few hundred requests and exhausted completely at 9,000 — and a
// collision silently skipped the vendor. New references use 6 digits
// (MM-100000..MM-999999). Existing 4-digit references stay valid: the
// prefix is unchanged and mmReferencePattern accepts any digit count.
func generateReference() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(900000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("MM-%06d", 100000+n.Int64()), nil
}

// createVendorRequestWithReference assigns a fresh public reference and
// retries on a reference collision (detected authoritatively by the DB
// unique constraint), instead of silently skipping the vendor.
func (s *Service) createVendorRequestWithReference(ctx context.Context, vr *VendorRequest) (*VendorRequest, error) {
	for attempt := 0; attempt < maxReferenceAttempts; attempt++ {
		ref, err := generateReference()
		if err != nil {
			return nil, fmt.Errorf("generating public reference: %w", err)
		}
		vr.PublicReference = ref
		created, err := s.routing.CreateVendorRequest(ctx, vr)
		if errors.Is(err, ErrDuplicateReference) {
			s.logger.Warn("public reference collision, retrying", "reference", ref, "attempt", attempt+1)
			continue
		}
		return created, err
	}
	return nil, fmt.Errorf("could not allocate a unique public reference after %d attempts", maxReferenceAttempts)
}

func buildVendorRequestMessage(ref string, req *request.BuyerRequest) string {
	msg := fmt.Sprintf("Majo Request #%s\n\nA buyer is looking for:\n\n%s", ref, derefStr(req.Product))
	if req.Variant != nil {
		msg += fmt.Sprintf("\n%s", *req.Variant)
	}
	if req.Condition != nil {
		msg += fmt.Sprintf("\n%s", *req.Condition)
	}
	if req.BudgetMax != nil {
		msg += fmt.Sprintf("\nUp to ₦%d", *req.BudgetMax)
	}
	if req.Quantity != nil {
		msg += fmt.Sprintf("\nQuantity: %d", *req.Quantity)
	}
	if req.Location != nil {
		msg += fmt.Sprintf("\n%s", *req.Location)
	}
	msg += "\n\nCan you supply this? (Available / Can Source / Unavailable)"
	return msg
}

func derefStr(s *string) string {
	if s == nil {
		return "unspecified item"
	}
	return *s
}
