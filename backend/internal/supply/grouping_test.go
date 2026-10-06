package supply

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

var t0 = time.Date(2026, 10, 6, 10, 0, 0, 0, time.UTC)

const window = 120 * time.Second

type spec struct {
	at             int // seconds after t0
	kind           CandidateKind
	text           string
	product, price bool
	signal         *uuid.UUID
}

func cands(specs ...spec) []Candidate {
	out := make([]Candidate, len(specs))
	for i, s := range specs {
		out[i] = Candidate{MessageID: uuid.New(), Kind: s.kind, MessageAt: t0.Add(time.Duration(s.at) * time.Second),
			ReceivedAt: t0.Add(time.Duration(s.at)*time.Second + time.Millisecond), Text: s.text,
			HasProduct: s.product, HasPrice: s.price, SignalID: s.signal, MediaSettled: true}
	}
	return out
}

func sizes(gs []Group) []int {
	out := make([]int, len(gs))
	for i, g := range gs {
		out[i] = len(g.Members)
	}
	return out
}

func eq(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSegment_PhotosThenCaptionAreOneSubmission(t *testing.T) {
	gs := Segment(cands(
		spec{at: 0, kind: CandidateMedia}, spec{at: 10, kind: CandidateMedia}, spec{at: 18, kind: CandidateMedia},
		spec{at: 25, kind: CandidateText, text: "Imported ladies bags ₦18,500", product: true, price: true},
	), nil, window)
	if !eq(sizes(gs), []int{4}) || !gs[0].Closed || !gs[0].HasText() {
		t.Fatalf("expected one closed submission of 4, got %v", sizes(gs))
	}
}

func TestSegment_WindowBoundary(t *testing.T) {
	in := Segment(cands(spec{at: 0, kind: CandidateMedia}, spec{at: 60, kind: CandidateMedia},
		spec{at: 119, kind: CandidateText, text: "bags 18,500", product: true, price: true}), nil, window)
	if !eq(sizes(in), []int{3}) {
		t.Fatalf("119s: expected one submission, got %v", sizes(in))
	}
	out := Segment(cands(spec{at: 0, kind: CandidateMedia},
		spec{at: 121, kind: CandidateText, text: "bags 18,500", product: true, price: true}), nil, window)
	if !eq(sizes(out), []int{1, 1}) || out[1].StartReason != StartWindowExceeded {
		t.Fatalf("121s: expected two submissions (window exceeded), got %v", sizes(out))
	}
	// Window is measured from the submission's FIRST message, not chained.
	chain := Segment(cands(spec{at: 0, kind: CandidateMedia}, spec{at: 100, kind: CandidateMedia}, spec{at: 200, kind: CandidateMedia}), nil, window)
	if !eq(sizes(chain), []int{2, 1}) {
		t.Fatalf("chained photos: expected [2 1], got %v", sizes(chain))
	}
	// The parameter, not the number 120, decides.
	if gs := Segment(cands(spec{at: 0, kind: CandidateMedia}, spec{at: 119, kind: CandidateMedia}), nil, 60*time.Second); len(gs) != 2 {
		t.Fatal("a 60s window must split at 119s")
	}
}

func TestSegment_MultipleCaptionsCombine(t *testing.T) {
	gs := Segment(cands(
		spec{at: 0, kind: CandidateMedia, text: "iPhone 13", product: true},
		spec{at: 20, kind: CandidateMedia, text: "128GB 620k", price: true},
	), nil, window)
	if !eq(sizes(gs), []int{2}) || !gs[0].HasProduct || !gs[0].HasPrice {
		t.Fatalf("captions completing each other must stay together, got %v", sizes(gs))
	}
}

func TestSegment_DistinctProductsStaySeparate(t *testing.T) {
	texts := Segment(cands(
		spec{at: 0, kind: CandidateText, text: "iPhone 13 620k", product: true, price: true},
		spec{at: 60, kind: CandidateText, text: "PS5 650k", product: true, price: true},
	), nil, window)
	if !eq(sizes(texts), []int{1, 1}) {
		t.Fatalf("two complete text products must be two submissions, got %v", sizes(texts))
	}
	captions := Segment(cands(
		spec{at: 0, kind: CandidateMedia, text: "bags 18,500", product: true, price: true},
		spec{at: 10, kind: CandidateMedia},
		spec{at: 30, kind: CandidateMedia, text: "shoes 9,000", product: true, price: true},
		spec{at: 40, kind: CandidateMedia},
	), nil, window)
	if !eq(sizes(captions), []int{2, 2}) || captions[1].StartReason != StartNewProduct {
		t.Fatalf("a new product+price caption starts a new submission, got %v", sizes(captions))
	}
	// Captions-last: the text ends the submission, later photos start a new one.
	last := Segment(cands(
		spec{at: 0, kind: CandidateMedia}, spec{at: 5, kind: CandidateMedia},
		spec{at: 10, kind: CandidateText, text: "bags 18,500", product: true, price: true},
		spec{at: 20, kind: CandidateMedia}, spec{at: 25, kind: CandidateText, text: "shoes 9,000", product: true, price: true},
	), nil, window)
	if !eq(sizes(last), []int{3, 2}) || last[1].StartReason != StartAfterText {
		t.Fatalf("expected [3 2], got %v", sizes(last))
	}
}

func TestSegment_NonSupplyMessageSeparates(t *testing.T) {
	cs := cands(spec{at: 0, kind: CandidateMedia}, spec{at: 30, kind: CandidateText, text: "bags 18,500", product: true, price: true})
	gs := Segment(cs, []Break{{At: t0.Add(15 * time.Second), ReceivedAt: t0.Add(15 * time.Second)}}, window)
	if !eq(sizes(gs), []int{1, 1}) || gs[1].StartReason != StartAfterBreak {
		t.Fatalf("a reply in between must separate, got %v", sizes(gs))
	}
}

func TestSegment_ExistingSignalIsNeverRegrouped(t *testing.T) {
	sig := uuid.New()
	gs := Segment(cands(
		spec{at: 0, kind: CandidateText, text: "bags 18,500", product: true, price: true, signal: &sig},
		spec{at: 20, kind: CandidateMedia}, spec{at: 30, kind: CandidateMedia},
	), nil, window)
	if !eq(sizes(gs), []int{1, 2}) || gs[0].SignalID == nil || *gs[0].SignalID != sig ||
		gs[1].SignalID != nil || gs[1].StartReason != StartAfterSignal || gs[1].HasText() {
		t.Fatalf("expected fixed signal group then an open media group, got %+v", gs)
	}
	if GroupOf(gs, gs[1].Members[1].MessageID) != 1 || GroupOf(gs, uuid.New()) != -1 {
		t.Fatal("GroupOf")
	}
}

func TestSegment_SettledAndOrderingTieBreak(t *testing.T) {
	cs := cands(spec{at: 0, kind: CandidateMedia}, spec{at: 0, kind: CandidateMedia})
	cs[1].MediaSettled = false
	gs := Segment(cs, nil, window)
	if len(gs) != 1 || gs[0].Settled() {
		t.Fatal("a group with an unfinished download is not settled")
	}
	if gs[0].Members[0].MessageID != cs[0].MessageID {
		t.Fatal("order must be preserved")
	}
}
