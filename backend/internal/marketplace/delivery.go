package marketplace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/offer"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
)

// staleSendingAfter: an offer left in SENDING longer than this (process
// crashed mid-send) may be reclaimed by the next delivery attempt.
// Reclaiming means a buyer could, rarely, receive the same option twice;
// it never means a lost option.
const staleSendingAfter = 2 * time.Minute

// ErrDeliveryFailed wraps a rejected WhatsApp send of an offer batch.
var ErrDeliveryFailed = errors.New("offer delivery failed")

// OfferDelivery builds and delivers buyer-facing offers.
type OfferDelivery struct {
	requests   request.Repository
	convs      conversation.Repository
	offerSvc   *offer.Service
	offers     offer.Repository
	delivery   offer.DeliveryRepository
	routing    routing.Repository
	selections SelectionReader
	messaging  Messenger
	events     event.Writer
	logger     *slog.Logger
	now        func() time.Time
	failures   OfferFailureListener
}

// OfferFailureListener is told when an automatic offer delivery was rejected
// by the channel (Phase 4C correction: the autonomous runtime retries it, so
// the operator is not the retry mechanism).
type OfferFailureListener interface {
	OfferDeliveryFailed(ctx context.Context, buyerRequestID uuid.UUID, err error)
}

// SetFailureListener wires automatic delivery recovery.
func (d *OfferDelivery) SetFailureListener(l OfferFailureListener) { d.failures = l }

func NewOfferDelivery(
	requests request.Repository,
	convs conversation.Repository,
	offerSvc *offer.Service,
	offers offer.Repository,
	delivery offer.DeliveryRepository,
	routingRepo routing.Repository,
	selections SelectionReader,
	messaging Messenger,
	events event.Writer,
	logger *slog.Logger,
) *OfferDelivery {
	return &OfferDelivery{
		requests: requests, convs: convs, offerSvc: offerSvc, offers: offers, delivery: delivery,
		routing: routingRepo, selections: selections, messaging: messaging, events: events, logger: logger,
		now: time.Now,
	}
}

// DeliveryResult reports what a delivery attempt did.
type DeliveryResult struct {
	Delivered      int    `json:"delivered"`
	Ordinals       []int  `json:"ordinals"`
	HighestOrdinal int    `json:"highest_ordinal"`
	Skipped        string `json:"skipped,omitempty"` // why nothing was attempted
}

// DeliverPendingOffers builds any newly eligible offers for the request and
// sends every not-yet-delivered one to the buyer in a single WhatsApp
// message. Safe to call any number of times: each offer is claimed by
// exactly one caller (DB-serialized) and delivered at most once on success.
//
// Returns request.ErrNotConfirmed for an unconfirmed request (nothing is
// built or sent), and ErrDeliveryFailed if WhatsApp rejected the send (the
// offers are returned to a deliverable state and the failure is recorded).
func (d *OfferDelivery) DeliverPendingOffers(ctx context.Context, buyerRequestID uuid.UUID) (*DeliveryResult, error) {
	req, err := d.requests.FindByID(ctx, buyerRequestID)
	if err != nil {
		return nil, fmt.Errorf("loading buyer request: %w", err)
	}
	if req == nil {
		return nil, fmt.Errorf("buyer request %s not found", buyerRequestID)
	}
	if err := req.EnsureConfirmed(); err != nil {
		return nil, err
	}

	// A decided request gets no further options.
	if sel, err := d.selections.FindSelectionByBuyerRequest(ctx, buyerRequestID); err != nil {
		return nil, fmt.Errorf("checking selection: %w", err)
	} else if sel != nil {
		return &DeliveryResult{Skipped: "already_selected"}, nil
	}

	if _, err := d.offerSvc.BuildForBuyerRequest(ctx, buyerRequestID); err != nil {
		return nil, fmt.Errorf("building offers: %w", err)
	}

	claimed, highest, err := d.delivery.ClaimForDelivery(ctx, buyerRequestID, staleSendingAfter)
	if err != nil {
		return nil, fmt.Errorf("claiming offers for delivery: %w", err)
	}
	if len(claimed) == 0 {
		return &DeliveryResult{HighestOrdinal: highest, Skipped: "nothing_new"}, nil
	}

	ids := make([]uuid.UUID, len(claimed))
	ordinals := make([]int, len(claimed))
	for i, o := range claimed {
		ids[i] = o.ID
		if o.PresentationOrdinal != nil {
			ordinals[i] = *o.PresentationOrdinal
		}
	}

	conv, err := d.convs.FindByID(ctx, req.ConversationID)
	if err != nil {
		d.releaseClaim(ctx, ids, "loading conversation: "+err.Error())
		return nil, fmt.Errorf("loading conversation: %w", err)
	}

	followUp := highest > len(claimed) // earlier options were already presented
	body := offer.FormatOfferDelivery(SummaryOf(req), claimed, highest, followUp)

	idStrs := make([]string, len(ids))
	for i, id := range ids {
		idStrs[i] = id.String()
	}
	reqID := req.ID
	sendCtx := conversation.WithOutboundPurpose(ctx, conversation.OutboundPurpose{
		Kind: "offer_delivery", EntityType: "buyer_request", EntityID: &reqID,
		Extra: map[string]any{"offer_ids": idStrs, "ordinals": ordinals},
	})
	if err := d.messaging.SendText(sendCtx, conv.ExternalID, body); err != nil {
		d.releaseClaim(ctx, ids, err.Error())
		for i, id := range ids {
			offerID := id
			if wErr := d.events.Write(ctx, &event.Event{
				EventType: event.OfferSendFailed, EntityType: "offer", EntityID: &offerID, ActorType: event.ActorSystem,
				Payload: event.Merge(req.FunnelDims(), offerDims(claimed[i]), map[string]any{
					"attempted_ordinal": ordinals[i], "error": err.Error(), "channel": conv.Channel, "outcome": "send_failed",
				}),
			}); wErr != nil {
				d.logger.Error("writing OFFER_SEND_FAILED event", "error", wErr)
			}
		}
		return nil, fmt.Errorf("%w: %v", ErrDeliveryFailed, err)
	}

	sentAt := d.now().UTC()
	if err := d.delivery.MarkDelivered(ctx, ids, sentAt, sentAt.Add(offer.SelectionWindow)); err != nil {
		// The buyer has the message; if this write fails the offers stay
		// SENDING and may be re-sent after staleSendingAfter (at-least-once).
		d.logger.Error("marking offers delivered", "buyer_request_id", buyerRequestID, "error", err)
	}

	for i, id := range ids {
		offerID := id
		if wErr := d.events.Write(ctx, &event.Event{
			EventType: event.OfferSent, EntityType: "offer", EntityID: &offerID, ActorType: event.ActorSystem,
			Payload: event.Merge(req.FunnelDims(), offerDims(claimed[i]), map[string]any{
				"ordinal": ordinals[i], "channel": conv.Channel, "batch_size": len(claimed), "outcome": "sent",
			}),
		}); wErr != nil {
			d.logger.Error("writing OFFER_SENT event", "error", wErr)
		}
	}
	convID := conv.ID
	if wErr := d.events.Write(ctx, &event.Event{
		EventType: event.MessageSent, EntityType: "conversation", EntityID: &convID, ActorType: event.ActorSystem,
		Payload: map[string]any{"kind": "offer_delivery", "buyer_request_id": buyerRequestID.String(), "ordinals": ordinals},
	}); wErr != nil {
		d.logger.Error("writing MESSAGE_SENT event", "error", wErr)
	}

	d.logger.Info("offers delivered to buyer", "buyer_request_id", buyerRequestID, "ordinals", ordinals)
	return &DeliveryResult{Delivered: len(claimed), Ordinals: ordinals, HighestOrdinal: highest}, nil
}

func (d *OfferDelivery) releaseClaim(ctx context.Context, ids []uuid.UUID, reason string) {
	if err := d.delivery.MarkDeliveryFailed(ctx, ids, reason); err != nil {
		d.logger.Error("releasing offer delivery claim", "error", err)
	}
}

// VendorResponseChanged implements routing.ResponseListener. Called after a
// vendor reply attached to a VendorRequest is recorded, corrected or
// re-associated:
//   - if the response is eligible, offers are built and delivered;
//   - if it is no longer eligible (e.g. corrected to UNAVAILABLE) and an
//     undecided offer exists for it, that offer is withdrawn so the buyer
//     can no longer select it.
//
// Errors are logged, never propagated: the vendor's message is already
// persisted, and delivery can be retried by the next trigger or by the
// operator (POST /api/requests/:id/offers/deliver).
func (d *OfferDelivery) VendorResponseChanged(ctx context.Context, resp *routing.VendorResponse) {
	if resp == nil || resp.VendorRequestID == nil {
		return
	}

	if !offer.IsEligible(resp) {
		d.withdrawOfferFor(ctx, resp)
		return
	}

	vr, err := d.routing.FindVendorRequestByID(ctx, *resp.VendorRequestID)
	if err != nil || vr == nil {
		d.logger.Error("loading vendor request for offer delivery", "vendor_request_id", *resp.VendorRequestID, "error", err)
		return
	}

	if _, err := d.DeliverPendingOffers(ctx, vr.BuyerRequestID); err != nil {
		if errors.Is(err, request.ErrNotConfirmed) {
			d.logger.Warn("vendor response for unconfirmed request — no offer built or sent", "buyer_request_id", vr.BuyerRequestID)
			return
		}
		d.logger.Error("delivering offers after vendor response", "buyer_request_id", vr.BuyerRequestID, "error", err)
		if errors.Is(err, ErrDeliveryFailed) && d.failures != nil {
			d.failures.OfferDeliveryFailed(ctx, vr.BuyerRequestID, err)
		}
	}
}

func (d *OfferDelivery) withdrawOfferFor(ctx context.Context, resp *routing.VendorResponse) {
	existing, err := d.offers.FindByVendorResponseID(ctx, resp.ID)
	if err != nil || existing == nil {
		return
	}
	switch existing.Status {
	case offer.StatusReviewed, offer.StatusSent, offer.StatusSending:
	default:
		return // already decided, withdrawn or retired
	}
	withdrawn := offer.StatusWithdrawn
	if _, err := d.offers.Update(ctx, existing.ID, offer.Fields{Status: &withdrawn}); err != nil {
		d.logger.Error("withdrawing offer", "offer_id", existing.ID, "error", err)
		return
	}
	offerID := existing.ID
	if wErr := d.events.Write(ctx, &event.Event{
		EventType: event.OfferWithdrawn, EntityType: "offer", EntityID: &offerID, ActorType: event.ActorSystem,
		Payload: event.Merge(offerDims(existing), map[string]any{
			"buyer_request_id": existing.BuyerRequestID.String(),
			"response_status":  string(resp.Status), "review_state": string(resp.ReviewState),
			"previous_offer_status": string(existing.Status), "outcome": "withdrawn",
		}),
	}); wErr != nil {
		d.logger.Error("writing OFFER_WITHDRAWN event", "error", wErr)
	}
}

// SummaryOf extracts the buyer-stated request description used to label
// offers in buyer-facing messages.
func SummaryOf(req *request.BuyerRequest) offer.RequestSummary {
	s := offer.RequestSummary{}
	if req.Product != nil {
		s.Product = *req.Product
	}
	if req.Variant != nil {
		s.Variant = *req.Variant
	}
	if req.Condition != nil {
		s.Condition = *req.Condition
	}
	return s
}

// offerDims returns the funnel dimensions of an offer (event payload keys).
func offerDims(o *offer.Offer) map[string]any {
	if o == nil {
		return map[string]any{}
	}
	d := map[string]any{
		"offer_id": o.ID.String(), "vendor_id": o.VendorID.String(), "offer_type": string(o.Type),
		"price": o.Price, "ranking_score": o.RankingScore,
	}
	if o.VendorResponseID != nil {
		d["vendor_response_id"] = o.VendorResponseID.String()
	}
	if o.VendorRequestID != nil {
		d["vendor_request_id"] = o.VendorRequestID.String()
	}
	if o.SupplyListingID != nil {
		d["supply_listing_id"] = o.SupplyListingID.String()
	}
	return d
}
