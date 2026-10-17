package deal

import (
	"strings"
	"testing"
	"time"
)

func sp(s string) *string { return &s }

func TestResolve(t *testing.T) {
	y, n, ny := sp(Yes), sp(No), sp(NotYet)
	cases := []struct {
		b, v  *string
		final bool
		want  Status
	}{
		{y, y, false, StatusConfirmed},
		{y, n, false, StatusDisputed},
		{n, y, false, StatusDisputed},
		{n, n, false, StatusDeclined},
		{y, nil, false, StatusAsked},
		{y, ny, false, StatusAsked},
		{nil, nil, false, StatusAsked},
		{y, nil, true, StatusBuyerOnly},
		{y, ny, true, StatusBuyerOnly},
		{nil, y, true, StatusVendorOnly},
		{n, nil, true, StatusDeclined},
		{nil, n, true, StatusDeclined},
		{ny, ny, true, StatusNoAnswer},
		{nil, nil, true, StatusNoAnswer},
		{y, y, true, StatusConfirmed},
	}
	for _, c := range cases {
		if got := Resolve(c.b, c.v, c.final); got != c.want {
			t.Fatalf("Resolve(%v, %v, final=%v) = %s, want %s", val(c.b), val(c.v), c.final, got, c.want)
		}
	}
	for _, st := range []Status{StatusConfirmed, StatusDisputed, StatusDeclined} {
		if !st.Terminal() {
			t.Fatalf("%s is terminal", st)
		}
	}
	for _, st := range []Status{StatusPending, StatusAsked, StatusBuyerOnly, StatusNoAnswer} {
		if st.Terminal() {
			t.Fatalf("%s is not terminal (a late answer can still settle it before closing)", st)
		}
	}
}

func TestParseAnswer(t *testing.T) {
	for in, want := range map[string]string{
		"YES": Yes, "yes o": Yes, "Yeah 👍": Yes, "I don buy am": Yes, "e don do": Yes, "yes, I bought it": Yes, "sold": Yes,
		"Yes!": Yes, "i don sell am": Yes, "No": No, "no o, e no work": No, "I no buy": No, "nah": No, "didn't buy": No,
		"not yet": NotYet, "Not yet o": NotYet, "tomorrow": NotYet, "e never reach": NotYet, "still waiting": NotYet,
		// not answers
		"how much be the iphone": "", "I need another phone": "", "": "", "yes 620k": "", "maybe": "",
		"yes I want to buy another laptop for my sister tomorrow morning please": "",
	} {
		if got := ParseAnswer(in); got != want {
			t.Fatalf("ParseAnswer(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRating(t *testing.T) {
	for in, want := range map[string]int{"5": 5, "1": 1, " 4 ": 4, "3 stars": 3, "4/5": 4, "⭐⭐⭐": 3, "i give am 5": 5, "5.": 5,
		"6": 0, "0": 0, "10": 0, "good": 0, "45": 0, "": 0, "4 or 5": 0} {
		if got := ParseRating(in); got != want {
			t.Fatalf("ParseRating(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestIntroductions(t *testing.T) {
	b := BuyerIntroduction("Mike Gadgets", "+234 800 000 0001", "MM-123456")
	for _, want := range []string{"Seller: Mike Gadgets", "https://wa.me/2348000000001", "MM-123456", "pay on delivery", "Majo doesn't hold or handle payments"} {
		if !strings.Contains(b, want) {
			t.Fatalf("buyer introduction missing %q:\n%s", want, b)
		}
	}
	amt := int64(620000)
	v := VendorIntroduction("iphone 13 128GB", &amt, "MM-123456", "2348011111111", false)
	if !strings.Contains(v, "iphone 13 128GB") || !strings.Contains(v, "₦620,000") || !strings.Contains(v, "MM-123456") ||
		!strings.Contains(v, "They'll message you") || strings.Contains(v, "2348011111111") {
		t.Fatalf("vendor introduction (buyer number not shared by default):\n%s", v)
	}
	if v2 := VendorIntroduction("x", nil, "", "2348011111111", true); !strings.Contains(v2, "https://wa.me/2348011111111") {
		t.Fatalf("shared when configured:\n%s", v2)
	}
	if WALink("abc") != "" || WALink("123") != "" {
		t.Fatalf("not a number, no link")
	}
}

func TestQuestions(t *testing.T) {
	if q := BuyerQuestion("small chops", "Mama Chops"); !strings.Contains(q, "did you buy the small chops from Mama Chops?") || !strings.Contains(q, "YES, NO or NOT YET") {
		t.Fatal(q)
	}
	if q := VendorQuestion("small chops", "MM-1"); !strings.Contains(q, "Did you sell the small chops to the Majo buyer (reference MM-1)?") {
		t.Fatal(q)
	}
	if q := RatingQuestion(""); !strings.Contains(q, "the seller") || !strings.Contains(q, "1 (bad) to 5 (great)") {
		t.Fatal(q)
	}
}

func TestNeedsAsk(t *testing.T) {
	now := time.Now()
	y, ny := sp(Yes), sp(NotYet)
	if !needsAsk(nil, nil, false, false) || needsAsk(nil, &now, false, false) || needsAsk(nil, nil, true, false) {
		t.Fatal("first round: ask once, never when unreachable")
	}
	if needsAsk(y, &now, false, true) || !needsAsk(ny, &now, false, true) || needsAsk(ny, &now, false, false) {
		t.Fatal("re-ask only after NOT YET, and only on the re-ask job")
	}
}

func TestDefaultConfig(t *testing.T) {
	c := DefaultConfig()
	if !c.Enabled || c.FollowUpDelay != 6*time.Hour || c.FreeDeals != 100 || c.ShareBuyerContact {
		t.Fatalf("%+v", c)
	}
}
