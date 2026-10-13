package workflow

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/channel"
	"github.com/middleman/backend/internal/matching"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/vendor"
)

func sp(s string) *string { return &s }
func ip(n int64) *int64   { return &n }

func emptyHistory() History { return History{Asked: map[string]int{}, Skipped: map[string]bool{}} }

func TestAssessClearRequestConfirms(t *testing.T) {
	r := &request.BuyerRequest{Product: sp("iphone 13"), Variant: sp("128GB"), BudgetMax: ip(650000), Location: sp("Benin City"), Confidence: 0.85}
	a := Assess(r, "", emptyHistory(), autonomy.DefaultParams())
	if !a.Clear || a.Missing != "" {
		t.Fatalf("clear request must confirm without questions, got %+v", a)
	}
}

func TestAssessAsksOnlyWhatTheNextActionNeeds(t *testing.T) {
	p := autonomy.DefaultParams()
	// No product -> ask product.
	a := Assess(&request.BuyerRequest{RawMessage: "I need that phone", Confidence: 0}, "", emptyHistory(), p)
	if a.Clear || a.Missing != FieldProduct {
		t.Fatalf("want product question, got %+v", a)
	}
	// Product but no location and none known -> ask location.
	a = Assess(&request.BuyerRequest{Product: sp("iphone 13"), Confidence: 0.6}, "", emptyHistory(), p)
	if a.Missing != FieldLocation {
		t.Fatalf("want location question, got %+v", a)
	}
	// Location known from context -> filled, not asked.
	a = Assess(&request.BuyerRequest{Product: sp("iphone 13"), Confidence: 0.6}, "Lagos", emptyHistory(), p)
	if !a.Clear || a.FillLocation != "Lagos" {
		t.Fatalf("want location from context, got %+v", a)
	}
	// Specific product: budget is never asked.
	a = Assess(&request.BuyerRequest{Product: sp("iphone 13"), Location: sp("Lagos"), Confidence: 0.6}, "", emptyHistory(), p)
	if !a.Clear {
		t.Fatalf("specific product with location is clear, got %+v", a)
	}
}

func TestAssessGenericProductAsksSizeThenBudgetOnce(t *testing.T) {
	p := autonomy.DefaultParams()
	r := &request.BuyerRequest{Product: sp("tv"), Location: sp("Benin City"), Confidence: 0.6}
	a := Assess(r, "", emptyHistory(), p)
	if a.Missing != FieldSpec || a.Question != "What size are you looking for, roughly?" {
		t.Fatalf("want size question first, got %+v", a)
	}
	r.Variant = sp("43 inch")
	h := emptyHistory()
	h.Asked[FieldSpec] = 1
	a = Assess(r, "", h, p)
	if a.Missing != FieldBudget {
		t.Fatalf("want budget question second, got %+v", a)
	}
	h.Asked[FieldBudget] = 1
	h.Skipped[FieldBudget] = true
	a = Assess(r, "", h, p)
	if !a.Clear {
		t.Fatalf("skipped budget must not be asked again, got %+v", a)
	}
}

func TestAssessListingOriginIsAlwaysClear(t *testing.T) {
	lid := uuid.New()
	r := &request.BuyerRequest{Product: sp("tv"), ListingID: &lid, Confidence: 1}
	if a := Assess(r, "", emptyHistory(), autonomy.DefaultParams()); !a.Clear {
		t.Fatalf("listing-origin demand names one listing: %+v", a)
	}
}

func cand(score, location int, within *bool) matching.Candidate {
	return matching.Candidate{Vendor: &vendor.Vendor{ID: uuid.New(), BusinessName: "V"}, Score: score,
		Breakdown: map[string]int{"location": location}, WithinBudget: within}
}

func TestRoutingPolicyThresholdFanOutBusyAndLocality(t *testing.T) {
	p := autonomy.DefaultParams()
	req := &request.BuyerRequest{Product: sp("iphone 13"), Location: sp("Benin City")}
	f := false
	low := cand(45, 20, nil)
	busy := cand(90, 20, nil)
	over := cand(95, 20, &f)
	a, b, c := cand(90, 20, nil), cand(80, 17, nil), cand(70, 20, nil)
	far := cand(85, 0, nil)
	open := map[uuid.UUID]int{busy.Vendor.ID: p.RouteVendorMaxOpen}
	plan := RoutingPolicy(req, []matching.Candidate{over, busy, a, far, b, c, low}, open, nil, p, time.Now())
	if len(plan.Route) != 3 {
		t.Fatalf("fan-out limit %d, got %d", p.RouteMaxVendors, len(plan.Route))
	}
	if plan.Route[0].Vendor.ID != a.Vendor.ID || plan.Route[1].Vendor.ID != b.Vendor.ID || plan.Route[2].Vendor.ID != c.Vendor.ID {
		t.Fatalf("over-budget evidence must rank after within-budget vendors")
	}
	if plan.Excluded[low.Vendor.ID.String()] != RouteBelowScore || plan.Excluded[busy.Vendor.ID.String()] != RouteVendorBusy {
		t.Fatalf("exclusions: %+v", plan.Excluded)
	}
	if plan.Excluded[over.Vendor.ID.String()] != RouteFanOutLimit {
		t.Fatalf("4th local vendor must be cut by fan-out: %+v", plan.Excluded)
	}
	if len(plan.NonLocal) != 1 || plan.NonLocal[0].Vendor.ID != far.Vendor.ID {
		t.Fatalf("vendor outside the area is held back, got %+v", plan.NonLocal)
	}
}

func TestRoutingPolicyNoLocationMeansEveryEligibleVendorIsLocal(t *testing.T) {
	req := &request.BuyerRequest{Product: sp("ps5")}
	plan := RoutingPolicy(req, []matching.Candidate{cand(70, 0, nil)}, nil, nil, autonomy.DefaultParams(), time.Now())
	if len(plan.Route) != 1 || len(plan.NonLocal) != 0 {
		t.Fatalf("got %+v", plan)
	}
}

func items() []ContextItem {
	now := time.Now()
	d := uuid.New()
	l1, l2 := uuid.New(), uuid.New()
	return []ContextItem{
		{Kind: ItemListing, Group: "delivery:1", At: now, DeliveryID: &d, ListingID: &l1, Ref: "L-AAA111", Product: "iPhone 13", Variant: "128GB black", Price: ip(120000)},
		{Kind: ItemListing, Group: "delivery:1", At: now, DeliveryID: &d, ListingID: &l2, Ref: "L-BBB222", Product: "iPhone 13", Variant: "128GB blue", Price: ip(145000)},
	}
}

func TestReferenceBlackOneResolvesToOne(t *testing.T) {
	ref := ParseReference("How much is the black one?")
	if ref.Intent != IntentPrice {
		t.Fatalf("intent: %+v", ref)
	}
	got := ResolveReference(ref, items())
	if len(got) != 1 || got[0].Ref != "L-AAA111" {
		t.Fatalf("want the black listing, got %+v", got)
	}
}

func TestReferenceTwoCandidatesAsksWhich(t *testing.T) {
	its := items()
	its[1].Variant = "128GB black"
	got := ResolveReference(ParseReference("how much is the black one"), its)
	if len(got) != 2 {
		t.Fatalf("two black phones -> ambiguous, got %d", len(got))
	}
	q, opts := WhichOptions(got, IntentPrice)
	if len(opts) != 2 || q == "" || !contains(q, "₦120,000") || !contains(q, "₦145,000") {
		t.Fatalf("question must name both prices: %q", q)
	}
	if o := MatchItemOption("the 145k one", opts); o == nil || o.Key != "2" {
		t.Fatalf("price answer must pick option 2, got %+v", o)
	}
	if o := MatchItemOption("1", opts); o == nil || o.Key != "1" {
		t.Fatalf("number answer, got %+v", o)
	}
}

func TestReferenceNotAboutContextFallsThrough(t *testing.T) {
	if ParseReference("I need an iPhone 13 128GB, budget 650k, Benin City").Intent != IntentNone {
		t.Fatalf("a complete new request is not a reference")
	}
	got := ResolveReference(ParseReference("how much is the samsung one"), items())
	if len(got) != 0 {
		t.Fatalf("named something never shown -> ordinary path, got %+v", got)
	}
}

func TestReferenceThatPhoneUsesNounAndFocus(t *testing.T) {
	its := items()
	its[1].Focus = true
	got := ResolveReference(ParseReference("I want that phone"), its)
	if len(got) != 1 || got[0].Ref != "L-BBB222" {
		t.Fatalf("focus wins for 'that phone', got %+v", got)
	}
	its[1].Focus = false
	got = ResolveReference(ParseReference("I want that one"), its)
	if len(got) != 2 {
		t.Fatalf("no focus, two in the latest group -> ask, got %d", len(got))
	}
}

func TestCheaperNeedsAPricedItem(t *testing.T) {
	ref := ParseReference("find me something cheaper")
	if ref.Intent != IntentCheaper {
		t.Fatalf("intent %+v", ref)
	}
	if got := ResolveReference(ref, items()[:1]); len(got) != 1 {
		t.Fatalf("got %+v", got)
	}
}

func TestAnswerPatch(t *testing.T) {
	req := &request.BuyerRequest{Product: sp("tv")}
	p, got := answerPatch(FieldSpec, "55 inch", &ai.ExtractionResult{}, req)
	if !got || p.Variant == nil || *p.Variant != "55 inch" {
		t.Fatalf("raw short answer becomes the spec: %+v", p)
	}
	p, got = answerPatch(FieldBudget, "around 300k", &ai.ExtractionResult{BudgetMax: ip(300000)}, req)
	if !got || p.BudgetMax == nil || *p.BudgetMax != 300000 {
		t.Fatalf("budget: %+v", p)
	}
	_, got = answerPatch(FieldBudget, "hmm", &ai.ExtractionResult{}, req)
	if got {
		t.Fatalf("unparseable budget is not an answer")
	}
	p, _ = answerPatch(FieldLocation, "Benin, budget 200k", &ai.ExtractionResult{Location: sp("benin city"), BudgetMax: ip(200000)}, req)
	if p.BudgetMax == nil || *p.Location != "benin city" {
		t.Fatalf("volunteered facts fill gaps: %+v", p)
	}
}

func TestSkipsAndYesNo(t *testing.T) {
	for _, s := range []string{"any", "Doesn't matter", "no budget", "anywhere"} {
		if !IsSkip(s) {
			t.Fatalf("%q should skip", s)
		}
	}
	if YesNo("Yes please") != 1 || YesNo("no thanks") != -1 || YesNo("maybe") != 0 {
		t.Fatalf("yes/no")
	}
}

func TestClassifySendError(t *testing.T) {
	// Text fallback for errors that are not *channel.ProviderError.
	cases := map[string]channel.FailureClass{
		"whatsapp api returned status 400: {\"error\":{\"code\":131047}}": channel.FailPolicy,
		"whatsapp api returned status 400: {\"error\":{\"code\":130429}}": channel.FailRateLimited,
		"whatsapp api returned status 503: unavailable":                   channel.FailUnavailable,
		"whatsapp api returned status 500: oops":                          channel.FailTransient,
		"whatsapp api returned status 429: rate":                          channel.FailRateLimited,
		"whatsapp api returned status 401: bad token":                     channel.FailAuthConfig,
		"whatsapp api returned status 400: {\"error\":{\"code\":999999}}": channel.FailUnknown,
		"calling whatsapp api: dial tcp: connection refused":              channel.FailTransient,
	}
	for msg, want := range cases {
		if got := ClassifySendError(errors.New(msg)); got != want {
			t.Fatalf("%q: got %s want %s", msg, got, want)
		}
	}
}

func TestVendorTrustGate(t *testing.T) {
	if (VendorTrust{Decided: 19, ApprovedUnchanged: 19}).Trusted() {
		t.Fatalf("needs 20 decisions")
	}
	if (VendorTrust{Decided: 20, ApprovedUnchanged: 18}).Trusted() {
		t.Fatalf("18/20 = 90%% < 95%%")
	}
	if !(VendorTrust{Decided: 20, ApprovedUnchanged: 19}).Trusted() {
		t.Fatalf("19/20 = 95%% is trusted")
	}
}

func TestClassifyWorkflow(t *testing.T) {
	if autonomy.Classify(autonomy.WorkflowFacts{}) != autonomy.ClassAutonomous {
		t.Fatal("plain")
	}
	if autonomy.Classify(autonomy.WorkflowFacts{Clarified: true}) != autonomy.ClassClarificationAssist {
		t.Fatal("clarified")
	}
	if autonomy.Classify(autonomy.WorkflowFacts{Clarified: true, OperatorRouted: true}) != autonomy.ClassOperatorIntervention {
		t.Fatal("manual routing is never autonomous")
	}
	if autonomy.Classify(autonomy.WorkflowFacts{OperatorRouted: true, SystemException: true}) != autonomy.ClassSystemFailure {
		t.Fatal("system failure first")
	}
	r := autonomy.NewRate(1, 4)
	if r.N != 1 || r.Of != 4 || r.Pct == nil || *r.Pct != 25 {
		t.Fatalf("rate %+v", r)
	}
	if autonomy.NewRate(0, 0).Pct != nil {
		t.Fatal("no denominator -> no percentage")
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}

func TestSpecInTextAvoidsAskingWhatWasSaid(t *testing.T) {
	cases := map[[2]string]string{
		{"I need a Samsung A54 in Benin City", "samsung"}: "A54",
		{"I need a 55 inch TV", "tv"}:                     "55 inch",
		{"iphone 15 pro max abeg", "iphone"}:              "15 pro max", // every model suffix is kept (Phase 4C hardening)
		{"I need a TV for my room", "tv"}:                 "",
	}
	for in, want := range cases {
		if got := SpecInText(in[0], in[1]); got != want {
			t.Fatalf("%q: got %q want %q", in[0], got, want)
		}
	}
	r := &request.BuyerRequest{Product: sp("samsung"), Location: sp("Benin City"), Confidence: 0.6, RawMessage: "I need a Samsung A54 in Benin City"}
	a := Assess(r, "", emptyHistory(), autonomy.DefaultParams())
	if !a.Clear || a.FillVariant != "A54" {
		t.Fatalf("stated model is used, not asked: %+v", a)
	}
	r.Variant = sp("A54")
	if a := Assess(r, "", emptyHistory(), autonomy.DefaultParams()); !a.Clear || a.FillVariant != "" {
		t.Fatalf("no budget question for a specific model: %+v", a)
	}
}

// Review findings: ordinary requests must never be read as references to
// what was shown, and answers must not be lost to the reference path.
func TestReferenceNeverHijacksNewRequests(t *testing.T) {
	offers := []ContextItem{{Kind: ItemOffer, Group: "offers:1", At: time.Now(), Ordinal: 1, Product: "iphone 12", Variant: "256GB", Price: ip(500000)}}
	for _, text := range []string{
		"I need a phone that has good battery",
		"I need an iPhone 13 that is 256gb",
		"How much is an iPhone 14 these days?",
		"I need a TV that is 55 inch",
	} {
		if got := ResolveReference(ParseReference(text), offers); len(got) != 0 {
			t.Fatalf("%q must be a new request, resolved to %+v", text, got)
		}
	}
	if got := ResolveReference(ParseReference("how much is that one now"), offers); len(got) != 1 {
		t.Fatalf("fillers do not block a real reference, got %d", len(got))
	}
}

func TestPlaceFromAnswer(t *testing.T) {
	for in, want := range map[string]string{"I need it in Lekki": "Lekki", "around Ikeja abeg": "Ikeja", "Lekki": "Lekki", "at Uniben gate.": "Uniben gate"} {
		if got := placeFromAnswer(in); got != want {
			t.Fatalf("%q -> %q want %q", in, got, want)
		}
	}
	p, got := answerPatch(FieldLocation, "I need it in Lekki", &ai.ExtractionResult{}, &request.BuyerRequest{})
	if !got || *p.Location != "Lekki" {
		t.Fatalf("location answer: %+v", p)
	}
}

func TestStrongYesOnlySelects(t *testing.T) {
	for _, s := range []string{"ok", "pls", "check", "sharp", "okay"} {
		if StrongYes(s) {
			t.Fatalf("%q is an acknowledgement, not a purchase decision", s)
		}
	}
	if !StrongYes("Yes") || !StrongYes("I'll take it") {
		t.Fatal("explicit yes selects")
	}
}
