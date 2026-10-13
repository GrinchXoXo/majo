package marketplace

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/distribution"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/offer"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/structid"
	"github.com/middleman/backend/internal/transaction"
	"github.com/middleman/backend/internal/workflow"
)

// JobEnqueuer runs work in the background (internal/jobs.Runner).
type JobEnqueuer interface {
	Enqueue(job func(context.Context))
}

// InboundBuyerMessage is a channel-neutral, already-persisted inbound
// message from a buyer. The channel adapter (WhatsApp webhook) has already
// verified, de-duplicated and stored it.
type InboundBuyerMessage struct {
	ConversationID uuid.UUID
	BuyerID        uuid.UUID
	MessageID      uuid.UUID
	ReplyTo        string // channel address to reply to (e.g. WhatsApp wa_id)
	Type           string // "text", "audio", "image", ...
	Text           string
	Media          *conversation.MediaRef
	// InteractionID is the structured ID of a tapped reply button, list row
	// or template quick reply (Phase 4B); "" for ordinary messages.
	InteractionID string
}

// DiscoveryInteractions handles structured discovery interactions and the
// STOP keyword (distribution.Service implements it).
type DiscoveryInteractions interface {
	HandleStructured(ctx context.Context, in distribution.Inbound)
	HandleStopKeyword(ctx context.Context, in distribution.Inbound) (handled bool)
}

// Buyer-facing copy. Kept together so wording can be reviewed in one place.
const (
	msgAck              = "Got it — I'm looking into that."
	msgUnsupportedMedia = "I can only read text messages for now 🙏 Please type what you're looking for."
	msgNoOpenOptions    = "I don't have any open options for you right now. Tell me what you're looking for and I'll search 👀"
	msgSelectionFailed  = "Sorry, I couldn't record that choice right now. Please try again in a moment."
	msgNextStep         = "Nothing has been paid yet — payment and delivery will be handled in the next step."
)

// BuyerInbound is the buyer-side entry point after the channel adapter:
// resolve context, then either run a deterministic marketplace action or
// the existing new-request pipeline.
type BuyerInbound struct {
	resolver  *ContextResolver
	requests  *request.Service
	txSvc     *transaction.Service
	offers    offer.Repository
	reqRepo   request.Repository
	routing   routing.Repository
	messaging Messenger
	events    event.Writer
	jobs      JobEnqueuer
	logger    *slog.Logger
	discovery DiscoveryInteractions // Phase 4B; nil = not wired
	auto      Autonomy              // Phase 4C correction; nil = not wired
}

// Autonomy is the context-resolution step of the autonomous runtime
// (workflow.Engine): answers to Majo's questions and references to things
// already shown are taken before the ordinary path.
type Autonomy interface {
	HandleBuyerText(ctx context.Context, m workflow.BuyerText) bool
}

// SetAutonomy wires the autonomous runtime.
func (b *BuyerInbound) SetAutonomy(a Autonomy) { b.auto = a }

// SetDiscovery wires structured discovery interactions (Phase 4B).
func (b *BuyerInbound) SetDiscovery(d DiscoveryInteractions) { b.discovery = d }

func NewBuyerInbound(
	resolver *ContextResolver,
	requests *request.Service,
	reqRepo request.Repository,
	txSvc *transaction.Service,
	offers offer.Repository,
	routingRepo routing.Repository,
	messaging Messenger,
	events event.Writer,
	jobs JobEnqueuer,
	logger *slog.Logger,
) *BuyerInbound {
	return &BuyerInbound{
		resolver: resolver, requests: requests, reqRepo: reqRepo, txSvc: txSvc, offers: offers,
		routing: routingRepo, messaging: messaging, events: events, jobs: jobs, logger: logger,
	}
}

// Handle processes one inbound buyer message.
//
// Precedence (Architecture Lock V2 §H): a structured interaction ID always
// wins; then the free-text offer ordinal; then the normal new-request
// pipeline. A structured ID never falls through to free text, so a tap can
// never create an unrelated BuyerRequest.
func (b *BuyerInbound) Handle(ctx context.Context, m InboundBuyerMessage) {
	if m.InteractionID != "" || structid.Looks(m.Text) || m.Type == "interactive" || m.Type == "button" {
		raw := m.InteractionID
		if raw == "" {
			raw = strings.TrimSpace(m.Text)
		}
		in := distribution.Inbound{BuyerID: m.BuyerID, ConversationID: m.ConversationID, MessageID: m.MessageID,
			ReplyTo: m.ReplyTo, RawID: raw, Tapped: m.InteractionID != ""}
		if b.discovery != nil {
			b.discovery.HandleStructured(ctx, in)
			return
		}
		b.unresolved(ctx, m, Resolution{}, "structured_interaction_unavailable")
		return
	}
	if m.Type == "text" && b.discovery != nil && distribution.IsStopKeyword(m.Text) &&
		b.discovery.HandleStopKeyword(ctx, distribution.Inbound{BuyerID: m.BuyerID, ConversationID: m.ConversationID,
			MessageID: m.MessageID, ReplyTo: m.ReplyTo, RawID: "keyword:stop"}) {
		return
	}
	if m.Type != "text" || strings.TrimSpace(m.Text) == "" {
		// Phase 3.1 input foundation: media is persisted (MediaRef in the
		// message content) but not processed. Reply instead of silence.
		b.reply(ctx, m, msgUnsupportedMedia, "unsupported_message_type", map[string]any{"message_type": m.Type})
		return
	}

	if b.auto != nil {
		bt := workflow.BuyerText{ConversationID: m.ConversationID, BuyerID: m.BuyerID, MessageID: m.MessageID, ReplyTo: m.ReplyTo, Text: m.Text}
		if b.auto.HandleBuyerText(ctx, bt) {
			return
		}
	}
	b.handleDefault(ctx, m)
}

func fromBuyerText(t workflow.BuyerText) InboundBuyerMessage {
	return InboundBuyerMessage{ConversationID: t.ConversationID, BuyerID: t.BuyerID, MessageID: t.MessageID, ReplyTo: t.ReplyTo, Type: "text", Text: t.Text}
}

// HandleDefault implements workflow.BuyerPaths: the ordinary text path for a
// message the autonomous runtime handed back.
func (b *BuyerInbound) HandleDefault(ctx context.Context, t workflow.BuyerText) {
	b.handleDefault(ctx, fromBuyerText(t))
}

// SelectOffer implements workflow.BuyerPaths: the buyer's explicit choice.
func (b *BuyerInbound) SelectOffer(ctx context.Context, t workflow.BuyerText, buyerRequestID, offerID uuid.UUID, ordinal int) {
	b.handleSelection(ctx, fromBuyerText(t), Resolution{Kind: ResolveSelectOffer, BuyerRequestID: buyerRequestID, OfferID: offerID, Ordinal: ordinal})
}

// handleDefault is the ordinary text path: an offer selection by number,
// an unresolvable selection-shaped reply, or a new request.
func (b *BuyerInbound) handleDefault(ctx context.Context, m InboundBuyerMessage) {
	res, err := b.resolver.Resolve(ctx, m.ConversationID, m.Text)
	if err != nil {
		// Resolution failure must not lose the message: fall back to the
		// pre-3.1 behaviour (new request), which goes to operator review.
		b.logger.Error("resolving inbound context, falling back to new request", "conversation_id", m.ConversationID, "error", err)
		res = Resolution{Kind: ResolveNewRequest}
	}

	switch res.Kind {
	case ResolveSelectOffer:
		b.handleSelection(ctx, m, res)
	case ResolveUnresolved:
		b.handleUnresolved(ctx, m, res)
	default:
		b.handleNewRequest(ctx, m)
	}
}

// handleNewRequest is the unchanged Phase 1 pipeline: immediate ack,
// BuyerRequest creation, background extraction -> NEEDS_REVIEW.
func (b *BuyerInbound) handleNewRequest(ctx context.Context, m InboundBuyerMessage) {
	b.reply(ctx, m, msgAck, "acknowledgement", nil)

	created, err := b.requests.CreateFromMessage(ctx, m.ConversationID, m.BuyerID, m.MessageID, m.Text)
	if err != nil {
		b.logger.Error("creating buyer request", "error", err)
		return
	}
	b.logger.Info("buyer request created, queuing extraction", "conversation_id", m.ConversationID, "request_id", created.ID)

	requestID := created.ID
	b.jobs.Enqueue(func(bgCtx context.Context) {
		b.requests.RunExtraction(bgCtx, requestID)
	})
}

func (b *BuyerInbound) handleSelection(ctx context.Context, m InboundBuyerMessage, res Resolution) {
	result, err := b.txSvc.Select(ctx, res.BuyerRequestID, res.OfferID, m.BuyerID, transaction.SourceWhatsApp)
	switch {
	case err == nil:
		chosen, _ := b.offers.FindByID(ctx, res.OfferID)
		var text string
		if result.AlreadyRecorded {
			text = fmt.Sprintf("You've already chosen option %d ✅%s\n\n%s", res.Ordinal, b.referenceSuffix(ctx, chosen), msgNextStep)
		} else {
			text = b.selectionConfirmation(ctx, res, chosen)
		}
		kind := "selection_confirmation"
		if result.AlreadyRecorded {
			kind = "selection_already_recorded"
		}
		b.reply(ctx, m, text, kind, map[string]any{"buyer_request_id": res.BuyerRequestID.String(), "offer_id": res.OfferID.String(), "ordinal": res.Ordinal})

	case errors.Is(err, transaction.ErrAlreadySelected):
		held := "another option"
		if result != nil && result.Selection != nil {
			if heldOffer, _ := b.offers.FindByID(ctx, result.Selection.OfferID); heldOffer != nil && heldOffer.PresentationOrdinal != nil {
				held = fmt.Sprintf("option %d", *heldOffer.PresentationOrdinal)
			}
		}
		text := fmt.Sprintf("You've already chosen %s for this request, so I can't switch to option %d here. "+
			"If you'd like something different, just tell me what you're looking for.", held, res.Ordinal)
		b.unresolved(ctx, m, res, "already_selected_other")
		b.reply(ctx, m, text, "selection_conflict", nil)

	case errors.Is(err, transaction.ErrOfferNotSelectable):
		text := fmt.Sprintf("Sorry, option %d is no longer available. Tell me what you're looking for and I'll search again 👀", res.Ordinal)
		b.unresolved(ctx, m, res, "not_selectable")
		b.reply(ctx, m, text, "selection_unavailable", nil)

	default:
		// ErrNotConfirmed / ErrNotRequestOwner / infrastructure errors:
		// should not happen on this path (the context came from this
		// buyer's own delivered offers), so log loudly and change nothing.
		b.logger.Error("offer selection failed", "buyer_request_id", res.BuyerRequestID, "offer_id", res.OfferID, "error", err)
		b.unresolved(ctx, m, res, "selection_error")
		b.reply(ctx, m, msgSelectionFailed, "selection_error", nil)
	}
}

func (b *BuyerInbound) handleUnresolved(ctx context.Context, m InboundBuyerMessage, res Resolution) {
	b.unresolved(ctx, m, res, res.Reason)

	var text string
	switch res.Reason {
	case ReasonNoOpenOptions:
		text = msgNoOpenOptions
	case ReasonOutOfRange:
		text = fmt.Sprintf("I don't have an option %d. %s", res.Ordinal, chooseHint(res.HighestOrdinal))
	default: // ambiguous
		text = "Just to be sure 🙏 " + chooseHint(res.HighestOrdinal)
	}
	b.reply(ctx, m, text, "selection_clarification", map[string]any{"reason": res.Reason})
}

func chooseHint(highest int) string {
	if highest <= 1 {
		return "Reply 1 if you want the option I sent."
	}
	return fmt.Sprintf("Please reply with the number of the option you want (1-%d).", highest)
}

func (b *BuyerInbound) selectionConfirmation(ctx context.Context, res Resolution, chosen *offer.Offer) string {
	var sb strings.Builder
	sb.WriteString("Got it ✅ I've recorded your choice:\n\n")
	if chosen != nil {
		summary := offer.RequestSummary{}
		if req, err := b.reqRepo.FindByID(ctx, res.BuyerRequestID); err == nil && req != nil {
			summary = SummaryOf(req)
		}
		ordinal := res.Ordinal
		line := offer.FormatOfferDelivery(summary, []*offer.Offer{chosen}, ordinal, false)
		// Reuse the delivery formatter's item rendering (title, price,
		// availability) — strip its header and reply instructions.
		sb.WriteString(itemBlock(line))
	} else {
		fmt.Fprintf(&sb, "Option %d\n", res.Ordinal)
	}
	sb.WriteString(b.referenceSuffix(ctx, chosen))
	sb.WriteString("\n\n")
	sb.WriteString(msgNextStep)
	return sb.String()
}

// itemBlock extracts the numbered item lines from a FormatOfferDelivery
// rendering (everything between the header and the reply instruction).
func itemBlock(rendered string) string {
	parts := strings.SplitN(rendered, "\n\n", 2)
	if len(parts) < 2 {
		return rendered
	}
	body := parts[1]
	if i := strings.LastIndex(body, "\n\n"); i >= 0 {
		body = body[:i]
	}
	return strings.TrimRight(body, "\n") + "\n"
}

// referenceSuffix returns "\nReference: MM-123456" using the existing public
// reference of the VendorRequest the offer came from. No new order-number
// format is invented; offers without a VendorRequest get no reference.
func (b *BuyerInbound) referenceSuffix(ctx context.Context, o *offer.Offer) string {
	if o == nil || o.VendorRequestID == nil {
		return ""
	}
	vr, err := b.routing.FindVendorRequestByID(ctx, *o.VendorRequestID)
	if err != nil || vr == nil || vr.PublicReference == "" {
		return ""
	}
	return "\nReference: " + vr.PublicReference
}

func (b *BuyerInbound) unresolved(ctx context.Context, m InboundBuyerMessage, res Resolution, reason string) {
	convID := m.ConversationID
	payload := map[string]any{"reason": reason, "text": m.Text, "ordinal": res.Ordinal, "highest_ordinal": res.HighestOrdinal}
	if res.BuyerRequestID != uuid.Nil {
		payload["buyer_request_id"] = res.BuyerRequestID.String()
	}
	if err := b.events.Write(ctx, &event.Event{
		EventType: event.BuyerReplyUnresolved, EntityType: "conversation", EntityID: &convID,
		ActorType: event.ActorBuyer, ActorID: &m.BuyerID, Payload: payload,
	}); err != nil {
		b.logger.Error("writing BUYER_REPLY_UNRESOLVED event", "error", err)
	}
}

func (b *BuyerInbound) reply(ctx context.Context, m InboundBuyerMessage, text, kind string, extra map[string]any) {
	convID := m.ConversationID
	sendCtx := conversation.WithOutboundPurpose(ctx, conversation.OutboundPurpose{
		Kind: kind, EntityType: "conversation", EntityID: &convID, Extra: extra,
	})
	if err := b.messaging.SendText(sendCtx, m.ReplyTo, text); err != nil {
		b.logger.Error("sending buyer reply", "kind", kind, "error", err)
		return
	}
	payload := map[string]any{"kind": kind}
	for k, v := range extra {
		payload[k] = v
	}
	if err := b.events.Write(ctx, &event.Event{
		EventType: event.MessageSent, EntityType: "conversation", EntityID: &convID,
		ActorType: event.ActorSystem, Payload: payload,
	}); err != nil {
		b.logger.Error("writing MESSAGE_SENT event", "error", err)
	}
}
