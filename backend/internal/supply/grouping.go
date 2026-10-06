package supply

// Phase 4A.2: deterministic grouping of consecutive vendor supply messages
// (photos, videos, captions, text) into one SupplySignal.
//
// No AI, no similarity, no clustering. The rules are explicit and live in
// one pure function (Segment) so they can be tested and explained:
//
//	Same vendor, same conversation, ordered by the vendor's own send time
//	(Meta's message timestamp), then receipt order.
//	A new submission starts when:
//	  1. the message is already evidence of a signal (history is never rewritten);
//	  2. it is more than `window` after the submission's FIRST message;
//	  3. a non-supply message from the vendor sits in between (a reply to a
//	     VendorRequest, an ambiguous photo, a voice note ...);
//	  4. the submission already ended with a standalone text message
//	     (captions-last: "photo, photo, 'bags 18,500'" is one submission and the
//	     text closes it);
//	  5. the message names a product AND a price and the submission already
//	     has both (a new product is being introduced).
//
// Whether text names a product / a price comes from the existing heuristic
// supply extractor, recorded once per message when it was classified.

import (
	"time"

	"github.com/google/uuid"
)

// CandidateKind is the kind of supply candidate message.
type CandidateKind string

const (
	CandidateText  CandidateKind = "text"
	CandidateMedia CandidateKind = "media"
)

// Candidate is one inbound vendor message accepted as supply content, plus
// the state grouping needs. Text holds the message text or media caption.
type Candidate struct {
	MessageID      uuid.UUID
	VendorID       uuid.UUID
	ConversationID uuid.UUID
	Kind           CandidateKind
	MessageAt      time.Time // vendor send time (Meta timestamp; receipt time as fallback)
	ReceivedAt     time.Time // when Majo persisted it (tie-break)
	Text           string
	HasProduct     bool
	HasPrice       bool
	Reason         string

	SignalID     *uuid.UUID // already evidence of this signal
	MediaAssetID *uuid.UUID // media: the stored asset, if any
	// MediaSettled: media download reached a final state (stored or failed).
	// Always true for text.
	MediaSettled bool
}

// Break is a non-supply inbound message from the vendor (it separates submissions).
type Break struct {
	At         time.Time
	ReceivedAt time.Time
}

// Group start reasons (logged and stored as signal metadata).
const (
	StartFirst            = "first_message"
	StartWindowExceeded   = "window_exceeded"
	StartAfterBreak       = "after_non_supply_message"
	StartAfterText        = "after_closing_text"
	StartNewProduct       = "new_product_and_price"
	StartAfterSignal      = "after_existing_signal"
	StartExistingEvidence = "existing_signal_evidence"
)

// Group is one submission: a run of candidates.
type Group struct {
	Members     []Candidate
	SignalID    *uuid.UUID // set when the members are already a signal's evidence
	StartReason string
	Closed      bool // ended by a standalone text message
	HasProduct  bool
	HasPrice    bool
}

// First is the group's first member.
func (g Group) First() Candidate { return g.Members[0] }

// HasText reports whether any member carries text (a caption or a text message).
func (g Group) HasText() bool {
	for _, m := range g.Members {
		if m.Text != "" {
			return true
		}
	}
	return false
}

// Settled reports whether every media member's download reached a final state.
func (g Group) Settled() bool {
	for _, m := range g.Members {
		if !m.MediaSettled {
			return false
		}
	}
	return true
}

// Contains reports whether the group includes the message.
func (g Group) Contains(id uuid.UUID) bool {
	for _, m := range g.Members {
		if m.MessageID == id {
			return true
		}
	}
	return false
}

func before(at1, rec1, at2, rec2 time.Time) bool {
	if !at1.Equal(at2) {
		return at1.Before(at2)
	}
	return rec1.Before(rec2)
}

// Segment splits time-ordered candidates of one vendor conversation into
// submissions. cands must be sorted by (MessageAt, ReceivedAt, MessageID).
func Segment(cands []Candidate, breaks []Break, window time.Duration) []Group {
	var groups []Group
	var cur *Group
	flush := func() {
		if cur != nil {
			groups = append(groups, *cur)
			cur = nil
		}
	}
	breakBetween := func(a, b Candidate) bool {
		for _, br := range breaks {
			if before(a.MessageAt, a.ReceivedAt, br.At, br.ReceivedAt) && before(br.At, br.ReceivedAt, b.MessageAt, b.ReceivedAt) {
				return true
			}
		}
		return false
	}

	var prev *Candidate
	for i := range cands {
		c := cands[i]
		brk := prev != nil && breakBetween(*prev, c)

		if c.SignalID != nil {
			if cur != nil && cur.SignalID != nil && *cur.SignalID == *c.SignalID && !brk {
				cur.Members = append(cur.Members, c)
			} else {
				flush()
				cur = &Group{SignalID: c.SignalID, StartReason: StartExistingEvidence, Members: []Candidate{c}}
			}
			prev = &cands[i]
			continue
		}

		reason := ""
		switch {
		case cur == nil:
			reason = StartFirst
		case brk:
			reason = StartAfterBreak
		case cur.SignalID != nil:
			reason = StartAfterSignal
		case cur.Closed:
			reason = StartAfterText
		case c.MessageAt.Sub(cur.First().MessageAt) > window:
			reason = StartWindowExceeded
		case c.HasProduct && c.HasPrice && cur.HasProduct && cur.HasPrice:
			reason = StartNewProduct
		}
		if reason != "" {
			flush()
			cur = &Group{StartReason: reason}
		}
		cur.Members = append(cur.Members, c)
		cur.HasProduct = cur.HasProduct || c.HasProduct
		cur.HasPrice = cur.HasPrice || c.HasPrice
		if c.Kind == CandidateText {
			cur.Closed = true
		}
		prev = &cands[i]
	}
	flush()
	return groups
}

// GroupOf returns the index of the group containing the message, or -1.
func GroupOf(groups []Group, messageID uuid.UUID) int {
	for i, g := range groups {
		if g.Contains(messageID) {
			return i
		}
	}
	return -1
}
