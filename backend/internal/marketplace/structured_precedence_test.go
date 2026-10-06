package marketplace

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/distribution"
	"github.com/middleman/backend/internal/request"
)

type recordingDiscovery struct {
	mu          sync.Mutex
	structured  []distribution.Inbound
	stops       int
	declineStop bool // simulate "not subscribed": HandleStopKeyword reports not handled
}

func (r *recordingDiscovery) HandleStructured(ctx context.Context, in distribution.Inbound) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.structured = append(r.structured, in)
}

func (r *recordingDiscovery) HandleStopKeyword(ctx context.Context, in distribution.Inbound) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stops++
	return !r.declineStop
}

// Phase 4B precedence: structured ID > free-text ordinal > new request.
// A tap never becomes a BuyerRequest or an offer selection, even while an
// offer context exists and even when its title looks like "1".
func TestPrecedence_StructuredIDAlwaysWins(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	disc := &recordingDiscovery{}
	h.inbound.SetDiscovery(disc)
	requests := h.countRequests()

	d := uuid.New()
	tap := func(id, title, typ string) {
		h.inbound.Handle(context.Background(), InboundBuyerMessage{ConversationID: h.convID, BuyerID: h.buyerID, MessageID: uuid.New(),
			ReplyTo: h.buyerPhone, Type: typ, Text: title, InteractionID: id})
	}
	tap("dd:"+d.String()+":l:L-1042AB:ask", "Ask about it", "interactive")
	tap("dd:"+d.String()+":show", "1", "button") // template quick reply titled "1"
	tap("opt_0", "Yes", "interactive")           // unknown id: still structured, never free text
	h.buyerSays("dd:" + d.String() + ":stop")    // typed structured text is not free text either

	if len(disc.structured) != 4 || h.countRequests() != requests || h.countSelections() != 0 {
		t.Fatalf("structured=%d requests %d->%d selections=%d", len(disc.structured), requests, h.countRequests(), h.countSelections())
	}
	if !disc.structured[0].Tapped || disc.structured[3].Tapped {
		t.Fatal("taps and typed IDs are distinguished (consent requires a real tap)")
	}
	h.buyerSays("STOP")
	if disc.stops != 1 || h.countRequests() != requests {
		t.Fatal("STOP is an opt-out command, never a request")
	}
	h.buyerSays("I need a PS5")
	if h.countRequests() != requests+1 {
		t.Fatal("ordinary text still creates a request")
	}
}

func TestPrecedence_StructuredWithoutDiscoveryChangesNothing(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	requests := h.countRequests()
	h.inbound.Handle(context.Background(), InboundBuyerMessage{ConversationID: h.convID, BuyerID: h.buyerID, MessageID: uuid.New(),
		ReplyTo: h.buyerPhone, Type: "interactive", Text: "Ask about it", InteractionID: "dd:" + uuid.NewString() + ":show"})
	if h.countRequests() != requests || h.countSelections() != 0 {
		t.Fatal("an unhandled structured interaction is unresolved, not a request")
	}
}

// A typed "stop" from a buyer who is NOT subscribed to discovery is not
// swallowed: it continues down the ordinary inbound path exactly as before
// Phase 4B (no behaviour change for existing traffic).
func TestStopKeyword_NotSubscribedFallsThrough(t *testing.T) {
	h := newHarness(t, request.StatusConfirmed)
	disc := &recordingDiscovery{declineStop: true}
	h.inbound.SetDiscovery(disc)
	requests := h.countRequests()
	h.buyerSays("stop")
	if disc.stops != 1 {
		t.Fatalf("discovery was asked once, got %d", disc.stops)
	}
	if h.countRequests() != requests+1 {
		t.Fatal("not handled by discovery -> the message is processed as before")
	}
}
