package marketplace

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/offer"
)

// ResolutionKind is what an inbound buyer message means in context.
// Deliberately tiny: one "new request" outcome, one marketplace action
// (offer selection), and "unresolved" — a selection-shaped reply that
// cannot be mapped deterministically, which changes no state. Future
// actions ("the cheapest one", WA Shopa confirmations, voice commands)
// attach here as new kinds; they are NOT implemented in Phase 3.1.
type ResolutionKind string

const (
	ResolveNewRequest  ResolutionKind = "NEW_REQUEST"
	ResolveSelectOffer ResolutionKind = "SELECT_OFFER"
	ResolveUnresolved  ResolutionKind = "UNRESOLVED"
)

// Why a selection-shaped reply could not be resolved.
const (
	ReasonNoOpenOptions = "no_open_options" // nothing has been offered in this conversation
	ReasonAmbiguous     = "ambiguous"       // e.g. "yes" — no deterministic mapping to one offer
	ReasonOutOfRange    = "out_of_range"    // a number that was never presented
)

// Resolution is the deterministic interpretation of one inbound message.
type Resolution struct {
	Kind           ResolutionKind
	BuyerRequestID uuid.UUID // context request (SELECT_OFFER / UNRESOLVED with context)
	OfferID        uuid.UUID // SELECT_OFFER only
	Ordinal        int       // the number the buyer sent, when any
	HighestOrdinal int       // highest option number presented for the context request
	Reason         string    // UNRESOLVED only
}

// ContextResolver maps an inbound buyer message onto either a new request
// or an action against the buyer's current marketplace context.
//
// The "current context" is derived from persisted state, not a separate
// conversation status: it is the buyer request in this conversation whose
// offers were most recently delivered. Offers carry stable presentation
// ordinals, so "2" means exactly the offer that was shown as "2."
// No context state can drift out of sync with what was actually sent.
type ContextResolver struct {
	offers   offer.Repository
	delivery offer.DeliveryRepository
}

func NewContextResolver(offers offer.Repository, delivery offer.DeliveryRepository) *ContextResolver {
	return &ContextResolver{offers: offers, delivery: delivery}
}

// Resolve never mutates state and never calls an AI model.
//
// Ordering matters and is intentional:
//  1. Text that is not selection-shaped is ALWAYS a new request, even while
//     an offer context exists — the buyer is never trapped in selection mode.
//  2. Selection-shaped text with no presented offers is UNRESOLVED
//     (no_open_options) rather than becoming a meaningless BuyerRequest
//     like raw_message="1".
//  3. A number maps to the presented offer with that ordinal, or is
//     UNRESOLVED (out_of_range).
//  4. An affirmative without a number ("yes", "this one") is UNRESOLVED
//     (ambiguous) — even when only one option exists — so that ambiguous
//     text can never trigger a selection.
func (r *ContextResolver) Resolve(ctx context.Context, conversationID uuid.UUID, text string) (Resolution, error) {
	reply := ParseSelectionReply(text)
	if reply.Kind == ReplyNone {
		return Resolution{Kind: ResolveNewRequest}, nil
	}

	requestID, err := r.delivery.FindLatestPresentedRequest(ctx, conversationID)
	if err != nil {
		return Resolution{}, fmt.Errorf("finding offer context: %w", err)
	}
	if requestID == nil {
		return Resolution{Kind: ResolveUnresolved, Reason: ReasonNoOpenOptions, Ordinal: reply.Ordinal}, nil
	}

	offers, err := r.offers.ListByBuyerRequest(ctx, *requestID)
	if err != nil {
		return Resolution{}, fmt.Errorf("loading context offers: %w", err)
	}
	byOrdinal := map[int]*offer.Offer{}
	highest := 0
	for _, o := range offers {
		if o.PresentationOrdinal == nil || o.SentAt == nil {
			continue // never shown to the buyer -> not addressable
		}
		byOrdinal[*o.PresentationOrdinal] = o
		if *o.PresentationOrdinal > highest {
			highest = *o.PresentationOrdinal
		}
	}

	res := Resolution{BuyerRequestID: *requestID, HighestOrdinal: highest, Ordinal: reply.Ordinal}
	if reply.Kind == ReplyAffirmative {
		res.Kind, res.Reason = ResolveUnresolved, ReasonAmbiguous
		return res, nil
	}
	o, ok := byOrdinal[reply.Ordinal]
	if !ok {
		res.Kind, res.Reason = ResolveUnresolved, ReasonOutOfRange
		return res, nil
	}
	res.Kind, res.OfferID = ResolveSelectOffer, o.ID
	return res, nil
}

// ReplyKind classifies the surface form of a buyer reply.
type ReplyKind int

const (
	ReplyNone        ReplyKind = iota // not a selection reply — treat as a new request
	ReplyNumber                       // an explicit option number
	ReplyAffirmative                  // "yes"/"this one" with no number — ambiguous
)

// SelectionReply is the parsed surface form.
type SelectionReply struct {
	Kind    ReplyKind
	Ordinal int
}

// selectionPattern matches a whole message that is nothing but an option
// number, optionally wrapped in a short selection phrase, e.g.
// "1", "#2", "option 1", "number 3", "no. 2", "I'll take 1", "I want 2",
// "give me 3 please", "I go take 1", "make I take 2", "2 abeg".
// It is anchored on both ends: "I need 2 iphones" or "2 bags of rice" are
// NOT selections — they fall through to the normal request pipeline.
var selectionPattern = regexp.MustCompile(
	`^(?:(?:i(?:'ll| will| go| fit)?|make i|let me|abeg|please|pls)\s+)?` +
		`(?:(?:take|want|pick|choose|select|go with|go for|give me|send me)\s+)?` +
		`(?:(?:the\s+)?(?:option|number|no\.?|num|choice)\s*)?` +
		`#?(\d{1,2})` +
		`(?:\s*(?:please|pls|abeg|biko|thanks|thank you))?$`)

// affirmatives are selection-intent replies that name no option. They are
// recognised only so they can be answered with a clarification instead of
// being turned into a junk BuyerRequest. Never mapped to an offer.
var affirmatives = map[string]bool{
	"yes": true, "yeah": true, "yea": true, "yep": true, "ok": true, "okay": true, "sure": true,
	"alright": true, "go ahead": true, "correct": true, "oya": true, "sharp": true,
	"this one": true, "that one": true, "na this one": true, "na that one": true,
	"i'll take it": true, "i will take it": true, "i want it": true, "i want this one": true,
	"i want that one": true, "i go take am": true, "go for it": true, "go for am": true,
	"buy it": true, "buy am": true, "i'll take this one": true, "i'll take that one": true,
}

// ParseSelectionReply deterministically classifies a buyer reply. Pure
// function — no context, no I/O, no AI.
func ParseSelectionReply(text string) SelectionReply {
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.TrimRight(t, ".!?)👍🙏 ")
	t = strings.Join(strings.Fields(t), " ")
	t = strings.ReplaceAll(t, "’", "'")
	if t == "" {
		return SelectionReply{Kind: ReplyNone}
	}
	if m := selectionPattern.FindStringSubmatch(t); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil { // 0 is parsed and later rejected as out_of_range
			return SelectionReply{Kind: ReplyNumber, Ordinal: n}
		}
	}
	if affirmatives[t] {
		return SelectionReply{Kind: ReplyAffirmative}
	}
	return SelectionReply{Kind: ReplyNone}
}
