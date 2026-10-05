package offer

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/matching"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
)

// Service builds buyer-facing Offers from VendorResponses (PRD 3 §15-18).
// Only responses that pass the eligibility gate ever become an Offer —
// raw or ambiguous vendor messages never reach the buyer directly.
type Service struct {
	offers   Repository
	routing  routing.Repository
	requests request.Repository
	events   event.Writer
	audits   audit.Writer
	logger   *slog.Logger
}

func NewService(offers Repository, routingRepo routing.Repository, requests request.Repository, events event.Writer, audits audit.Writer, logger *slog.Logger) *Service {
	return &Service{offers: offers, routing: routingRepo, requests: requests, events: events, audits: audits, logger: logger}
}

// eligible implements the PRD 3 §15 gate: a VendorResponse becomes an
// Offer only if it carries a real business classification (not
// UNCLASSIFIED, not UNAVAILABLE — a vendor saying "no" is not an offer)
// and has been reviewed (REVIEWED or CORRECTED, never NEEDS_REVIEW).
func eligible(resp *routing.VendorResponse) bool { return IsEligible(resp) }

// IsEligible is the exported PRD 3 §15 gate, also used by the Phase 3.1
// offer loop to withdraw offers whose source response was later corrected
// into an ineligible state.
func IsEligible(resp *routing.VendorResponse) bool {
	if resp.Status == routing.ResponseUnclassified || resp.Status == routing.ResponseUnavailable {
		return false
	}
	if resp.ReviewState == routing.ReviewNeedsReview {
		return false
	}
	return true
}

func offerType(status routing.ResponseStatus, exactProductMatch bool) Type {
	switch status {
	case routing.ResponseAvailable:
		if exactProductMatch {
			return TypeExact
		}
		return TypePartial
	case routing.ResponseAlternative:
		return TypeAlternative
	case routing.ResponseCanSource:
		return TypeCanSource
	default:
		return TypePartial
	}
}

// BuildForBuyerRequest scans every VendorRequest/VendorResponse pair for
// a confirmed BuyerRequest and creates an Offer for each eligible,
// not-yet-converted response (idempotent — safe to call repeatedly as
// more responses arrive). Returns the full current, ranked offer list.
func (s *Service) BuildForBuyerRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*Offer, error) {
	req, err := s.requests.FindByID(ctx, buyerRequestID)
	if err != nil {
		return nil, fmt.Errorf("loading buyer request: %w", err)
	}
	if req == nil {
		return nil, fmt.Errorf("buyer request %s not found", buyerRequestID)
	}
	// Phase 3.1 §3A-2: offers are marketplace execution — never generated
	// for an unconfirmed request.
	if err := req.EnsureConfirmed(); err != nil {
		return nil, err
	}

	return s.buildForRequest(ctx, req)
}

// BuyerRequestLabel returns the buyer-facing product label for a
// BuyerRequest — the product the buyer actually asked for, which is
// known/stated information regardless of whether any individual vendor
// restated it in their reply.
func (s *Service) BuyerRequestLabel(ctx context.Context, buyerRequestID uuid.UUID) (string, error) {
	req, err := s.requests.FindByID(ctx, buyerRequestID)
	if err != nil {
		return "", fmt.Errorf("loading buyer request: %w", err)
	}
	if req == nil || req.Product == nil {
		return "your request", nil
	}
	return *req.Product, nil
}

func (s *Service) buildForRequest(ctx context.Context, req *request.BuyerRequest) ([]*Offer, error) {
	buyerRequestID := req.ID

	vendorRequests, err := s.routing.ListVendorRequestsByBuyerRequest(ctx, buyerRequestID)
	if err != nil {
		return nil, fmt.Errorf("loading vendor requests: %w", err)
	}

	for _, vr := range vendorRequests {
		resp, err := s.routing.FindResponseByVendorRequest(ctx, vr.ID)
		if err != nil || resp == nil {
			continue
		}
		if !eligible(resp) {
			continue
		}

		existing, err := s.offers.FindByVendorResponseID(ctx, resp.ID)
		if err != nil {
			s.logger.Error("checking existing offer for response", "response_id", resp.ID, "error", err)
			continue
		}
		if existing != nil {
			continue // already converted — idempotent
		}

		score, reasons, exactMatch := rankOffer(req, resp)

		o := &Offer{
			BuyerRequestID: buyerRequestID, VendorRequestID: &vr.ID, VendorResponseID: &resp.ID,
			VendorID: vr.VendorID, Type: offerType(resp.Status, exactMatch), Status: StatusReviewed,
			Product: resp.Product, Variant: resp.Variant, Condition: resp.Condition,
			Price: resp.Price, Quantity: resp.Quantity, Location: resp.Location, Notes: resp.Notes,
			RankingScore: score, RankingReasons: reasons, Confidence: resp.Confidence,
		}

		created, err := s.offers.Create(ctx, o)
		if errors.Is(err, ErrAlreadyExists) {
			continue // a concurrent build created it first — one logical Offer per response
		}
		if err != nil {
			s.logger.Error("creating offer", "response_id", resp.ID, "error", err)
			continue
		}

		if err := s.events.Write(ctx, &event.Event{
			EventType: event.OfferCreated, EntityType: "offer", EntityID: &created.ID,
			ActorType: event.ActorSystem, Payload: map[string]any{"vendor_response_id": resp.ID.String(), "score": score},
		}); err != nil {
			s.logger.Error("writing OFFER_CREATED event", "error", err)
		}
	}

	return s.offers.ListByBuyerRequest(ctx, buyerRequestID)
}

// rankOffer implements PRD 3 §18's deterministic, explainable ranking,
// built directly on the Phase 2 matching primitives (product/location
// normalization) rather than a second scoring system.
func rankOffer(req *request.BuyerRequest, resp *routing.VendorResponse) (score int, reasons []string, exactProductMatch bool) {
	if req.Product != nil && resp.Product != nil {
		reqNorm := matching.NormalizeProduct(*req.Product)
		respNorm := matching.NormalizeProduct(*resp.Product)
		if reqNorm == respNorm {
			score += 30
			reasons = append(reasons, "+30 exact product")
			exactProductMatch = true
		} else if strings.Contains(respNorm, reqNorm) || strings.Contains(reqNorm, respNorm) {
			score += 15
			reasons = append(reasons, "+15 related product")
		}
	} else if req.Product != nil && resp.Product == nil && resp.Status == routing.ResponseAvailable {
		// The vendor didn't restate the product name, but this response
		// is a direct AVAILABLE reply to a VendorRequest that was
		// specifically about req.Product — that linkage is structurally
		// guaranteed (Phase 2's VendorRequest/VendorResponse pairing),
		// not an assumption we're inventing. This affects ranking/type
		// only; it never populates Offer.Product itself with text the
		// vendor didn't say (that would be hallucination at the
		// persisted/buyer-facing layer, which is a different concern).
		score += 30
		reasons = append(reasons, "+30 exact product (implied by direct reply to request)")
		exactProductMatch = true
	}

	if req.Variant != nil && resp.Variant != nil && strings.EqualFold(*req.Variant, *resp.Variant) {
		score += 20
		reasons = append(reasons, "+20 exact variant")
	}

	switch resp.Status {
	case routing.ResponseAvailable:
		score += 10
		reasons = append(reasons, "+10 available now")
	case routing.ResponseCanSource:
		score += 5
		reasons = append(reasons, "+5 can source")
	}

	if resp.Price != nil {
		withinBudget := true
		if req.BudgetMax != nil && *resp.Price > *req.BudgetMax {
			withinBudget = false
		}
		if withinBudget {
			score += 20
			reasons = append(reasons, "+20 within budget")
		}
	}

	if req.Condition != nil && resp.Condition != nil && strings.EqualFold(*req.Condition, *resp.Condition) {
		score += 10
		reasons = append(reasons, "+10 matching condition")
	}

	if req.Location != nil && resp.Location != nil &&
		matching.NormalizeLocation(*req.Location) == matching.NormalizeLocation(*resp.Location) {
		score += 10
		reasons = append(reasons, "+10 same location")
	}

	return score, reasons, exactProductMatch
}

// FormatBuyerMessage renders a concise, buyer-facing summary of offers
// (PRD 3 §19/§34) — no internal scores, no operator metadata, no raw AI
// output. Purely commercial information.
func FormatBuyerMessage(productLabel string, offers []*Offer) string {
	if len(offers) == 0 {
		return "Sorry, I couldn't find any options for that yet. I'll keep looking."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "I found %d option(s) for %s:\n\n", len(offers), productLabel)
	for i, o := range offers {
		fmt.Fprintf(&b, "%d. ", i+1)
		if o.Price != nil {
			fmt.Fprintf(&b, "₦%s\n", formatNaira(*o.Price))
		} else {
			b.WriteString("Price on request\n")
		}
		switch o.Type {
		case TypeCanSource:
			b.WriteString("   Can source\n")
		case TypeAlternative:
			b.WriteString("   Alternative option")
			if o.Variant != nil {
				fmt.Fprintf(&b, " (%s)", *o.Variant)
			}
			b.WriteString("\n")
		default:
			b.WriteString("   Available now\n")
		}
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("Reply with the number (1-%d) to choose.", len(offers)))
	return b.String()
}

func formatNaira(amount int64) string {
	s := fmt.Sprintf("%d", amount)
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}

// RequestSummary is the buyer-facing description of what the buyer asked
// for, used to label offers. Only buyer-stated information — never internal.
type RequestSummary struct {
	Product   string
	Variant   string
	Condition string
}

// FormatOfferDelivery renders the buyer-facing WhatsApp message for a batch
// of newly delivered offers (Phase 3.1). Each offer is shown with its
// stable PresentationOrdinal, so "1" always means the same offer for this
// request even when later batches add options 2, 3, ...
//
// Only commercial information appears: product, variant, condition, price
// and availability. Never scores, ranking reasons, confidence, vendor IDs,
// database IDs or operator metadata.
//
// For EXACT and CAN_SOURCE offers the vendor answered *this* request, so
// when the vendor didn't restate variant/condition, the buyer's own stated
// variant/condition is shown (the vendor confirmed that request). For
// ALTERNATIVE/PARTIAL offers only what the vendor actually said is shown.
func FormatOfferDelivery(req RequestSummary, batch []*Offer, highestOrdinal int, followUp bool) string {
	var b strings.Builder
	noun := "option"
	if len(batch) > 1 {
		noun = "options"
	}
	product := req.Product
	if product == "" {
		product = "your request"
	}
	switch {
	case followUp && len(batch) == 1:
		fmt.Fprintf(&b, "I found another option for %s 👀\n\n", product)
	case followUp:
		fmt.Fprintf(&b, "I found %d more options for %s 👀\n\n", len(batch), product)
	default:
		fmt.Fprintf(&b, "I found %d %s for %s 👀\n\n", len(batch), noun, product)
	}

	for _, o := range batch {
		ordinal := 0
		if o.PresentationOrdinal != nil {
			ordinal = *o.PresentationOrdinal
		}
		fmt.Fprintf(&b, "%d. %s\n", ordinal, offerTitle(req, o))
		if o.Price != nil {
			fmt.Fprintf(&b, "   ₦%s\n", formatNaira(*o.Price))
		} else {
			b.WriteString("   Price on request\n")
		}
		details := []string{availabilityLabel(o.Type)}
		if c := offerCondition(req, o); c != "" {
			details = append(details, strings.ToLower(c))
		}
		fmt.Fprintf(&b, "   %s\n\n", strings.Join(details, " · "))
	}

	if highestOrdinal <= 1 {
		b.WriteString("Reply 1 if you want this one.")
	} else {
		fmt.Fprintf(&b, "Reply with the number (1-%d) of the one you want.", highestOrdinal)
	}
	return b.String()
}

func offerTitle(req RequestSummary, o *Offer) string {
	product := req.Product
	if o.Product != nil && *o.Product != "" {
		product = *o.Product
	}
	if product == "" {
		product = "Item"
	}
	variant := ""
	if o.Variant != nil && *o.Variant != "" {
		variant = *o.Variant
	} else if o.Type == TypeExact || o.Type == TypeCanSource {
		variant = req.Variant
	}
	if variant != "" && !strings.Contains(strings.ToLower(product), strings.ToLower(variant)) {
		return product + " " + variant
	}
	return product
}

func offerCondition(req RequestSummary, o *Offer) string {
	if o.Condition != nil && *o.Condition != "" {
		return *o.Condition
	}
	if o.Type == TypeExact || o.Type == TypeCanSource {
		return req.Condition
	}
	return ""
}

func availabilityLabel(t Type) string {
	switch t {
	case TypeCanSource:
		return "Can be sourced"
	case TypeAlternative:
		return "Alternative to what you asked for"
	case TypePartial:
		return "Available now (similar item)"
	default:
		return "Available now"
	}
}

// FormatNaira exposes the existing naira formatter to the buyer-facing
// messaging in other packages (selection confirmation).
func FormatNaira(amount int64) string { return formatNaira(amount) }
