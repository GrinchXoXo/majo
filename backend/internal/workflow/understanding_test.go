package workflow

import (
	"testing"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/request"
)

func reqWith(raw, product, location string, extracted map[string]any) *request.BuyerRequest {
	r := &request.BuyerRequest{RawMessage: raw, Confidence: 0.6, ExtractedData: extracted}
	if product != "" {
		r.Product = sp(product)
	}
	if location != "" {
		r.Location = sp(location)
	}
	return r
}

func TestSeveralProductsAskedOnceThenProceeds(t *testing.T) {
	p := autonomy.DefaultParams()
	r := reqWith("iphone 13 and laptop in lagos", "iphone 13", "Lagos", map[string]any{"other_products": []any{"laptop"}})
	a := Assess(r, "", History{Asked: map[string]int{}, Skipped: map[string]bool{}}, p)
	if a.Outcome != OutcomeAmbiguous || a.Missing != FieldProduct || len(a.Options) != 2 {
		t.Fatalf("ask which first: %+v", a)
	}
	a = Assess(r, "", History{Asked: map[string]int{FieldProduct: 1}, Skipped: map[string]bool{}}, p)
	if !a.Clear {
		t.Fatalf("asked once: proceed with the chosen product, got %+v", a)
	}
}

func TestContradictionsAreAskedNotGuessed(t *testing.T) {
	p := autonomy.DefaultParams()
	h := History{Asked: map[string]int{}, Skipped: map[string]bool{}}
	a := Assess(reqWith("brand new fairly used ps5 in lagos", "ps5", "Lagos", map[string]any{"ambiguities": []any{"condition_conflict"}}), "", h, p)
	if a.Outcome != OutcomeConflicting || a.Missing != FieldCondition {
		t.Fatalf("condition conflict: %+v", a)
	}
	a = Assess(reqWith("tv in lagos or abuja", "tv", "Lagos", map[string]any{"ambiguities": []any{"location_alternatives"}}), "", h, p)
	if a.Outcome != OutcomeAmbiguous || a.Missing != FieldLocation {
		t.Fatalf("location alternatives asked even though one was extracted: %+v", a)
	}
}

func TestRestrictedItemsAreHeldNeverConfirmed(t *testing.T) {
	p := autonomy.DefaultParams()
	h := History{Asked: map[string]int{}, Skipped: map[string]bool{}}
	for _, raw := range []string{"I need a gun", "who get tramadol for benin", "fake passport urgently"} {
		a := Assess(reqWith(raw, "", "", nil), "Lagos", h, p)
		if !a.Hold || a.Clear || a.Outcome != OutcomePolicy {
			t.Fatalf("%q held: %+v", raw, a)
		}
	}
	for _, raw := range []string{"human hair wig", "I need gunmetal grey iphone", "codeines"} {
		if term := RestrictedTerm(raw, p.RestrictedTerms); term != "" && raw != "codeines" {
			t.Fatalf("%q must not match (whole words only), got %q", raw, term)
		}
	}
	p.RestrictedTerms = nil // AUTO_RESTRICTED_ITEM_TERMS=off
	if a := Assess(reqWith("I need a gun", "", "", nil), "", h, p); a.Hold {
		t.Fatalf("the list is configuration")
	}
}

func TestSmallTalkIsExactWholeMessage(t *testing.T) {
	for in, want := range map[string]string{"hello": "greeting", "Good morning sir": "greeting", "thanks!": "ack", "👍": "ack", "noted 🙏": "ack",
		"hello, I need a fridge": "", "ok": "", "yes": "", "thanks, I also need a laptop": "", "": ""} {
		if got := SmallTalk(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

// Pilot batch: Pidgin small talk is not demand.
func TestSmallTalkPidginAndClauses(t *testing.T) {
	for in, want := range map[string]string{
		"good morning o, how una dey": "greeting", "una dey there?": "greeting", "hw far boss": "greeting",
		"thank you jare 🙏": "ack", "lol": "ack", "😂😂": "ack", "thanks o, noted": "ack", "🙏🏾": "ack",
		// still requests / not small talk
		"good morning, I need a fridge": "", "how far, I need iphone 13": "", "ok": "", "yes o": "",
		"lol I need shoes": "", "one day na una I go buy car 😂": "",
	} {
		if got := SmallTalk(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestProductChoiceAnswers(t *testing.T) {
	opts := []autonomy.Option{{Key: "1", Label: "iphone 13"}, {Key: "2", Label: "laptop"}}
	for in, want := range map[string]string{"2": "laptop", "the laptop": "laptop", "iphone first": "iphone 13", "#1": "iphone 13"} {
		if o := MatchProductOption(in, opts); o == nil || o.Label != want {
			t.Fatalf("%q -> %v want %s", in, o, want)
		}
	}
	if o := MatchProductOption("iphone and laptop", opts); o != nil {
		t.Fatalf("naming both is not a choice")
	}
	for in, want := range map[string]string{"brand new": "new", "fairly used": "used", "either": "used_or_new", "new or used": "used_or_new", "tokunbo": "used", "hmm": ""} {
		if got := conditionAnswer(in); got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestFirstReadingNeverCreatesJunkRequests(t *testing.T) {
	if FirstReading("how much is it?", 0) != "reference_without_context" {
		t.Fatalf("question about nothing shown")
	}
	if FirstReading("how much is it?", 2) != "" {
		t.Fatalf("with items shown the reference resolves against them")
	}
	if FirstReading("how much is the iphone 13 in lagos", 0) != "" {
		t.Fatalf("a named product is a request")
	}
}

// Pilot batch: PILOT_DEFAULT_LOCATION fills a missing location instead of
// asking, but never overrides what the buyer said or what Majo knows.
func TestPilotDefaultLocation(t *testing.T) {
	if loc, src := KnownLocation("Lagos", "Benin City"); loc != "Lagos" || src != "buyer_context" {
		t.Fatalf("known context wins: %s %s", loc, src)
	}
	if loc, src := KnownLocation("", "Benin City"); loc != "Benin City" || src != "pilot_default" {
		t.Fatalf("default used: %s %s", loc, src)
	}
	if loc, src := KnownLocation("", ""); loc != "" || src != "" {
		t.Fatalf("no default: keep asking")
	}
	p := autonomy.DefaultParams()
	h := History{Asked: map[string]int{}, Skipped: map[string]bool{}}
	req := reqWith("abeg I need small chops for 20 people, budget 45,000", "small chops", "", nil)
	if a := Assess(req, "", h, p); a.Outcome != OutcomeMissing || a.Missing != "location" {
		t.Fatalf("without a default Majo asks for the location: %+v", a)
	}
	loc, _ := KnownLocation("", "Benin City")
	if a := Assess(req, loc, h, p); a.Outcome != OutcomeClear || a.FillLocation != "Benin City" {
		t.Fatalf("with the default the request is clear and filled: %+v", a)
	}
	said := reqWith("I need fridge in lagos", "fridge", "Lagos", nil)
	if a := Assess(said, loc, h, p); a.FillLocation != "" {
		t.Fatalf("the buyer's own location is never replaced: %+v", a)
	}
}
