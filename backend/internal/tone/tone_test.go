package tone

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Real buyer messages, as the templates produce them.
const (
	offerMsg = "I found 2 options for iphone 13 👀\n\n1. Mike Gadgets — iphone 13 128GB\n   ₦620,000\n   In stock · used\n\n" +
		"2. Tunde Phones — iphone 13\n   Price on request\n   Can source\n\nReply with the number (1-2) of the one you want."
	dealQ    = "Hi 👋 did you buy the small chops from Mama Chops? Reply YES, NO or NOT YET."
	introMsg = "Great choice! 🎉\n\nSeller: Mike Gadgets\nChat with them: https://wa.me/2348000000001\nReference: MM-123456\n\n" +
		"Tip: pay on delivery or after you see the item. Majo doesn't hold or handle payments."
	budgetQ  = "What's your budget for the office chair? A rough range is fine, like 50k-80k."
	searchAk = "Thanks! I'm searching for sellers near Unilag for your laptop now. I'll message you when someone replies."
	lateMsg  = "Earlier update: I couldn't find a seller in Yaba. Can I search in nearby areas too? Reply YES or NO."
)

func TestMask(t *testing.T) {
	cases := []struct {
		text  string
		keep  []string
		want  string
		facts []string
	}{
		{offerMsg, []string{"iphone 13"},
			"I found [[F1]] options for [[F2]] 👀\n\n[[F3]]\n   [[F4]]\n   [[F5]]\n\n[[F6]]\n   [[F7]]\n   [[F8]]\n\nReply with the number ([[F9]]) of the one you want.",
			[]string{"2", "iphone 13", "1. Mike Gadgets — iphone 13 128GB", "₦620,000", "In stock · used", "2. Tunde Phones — iphone 13", "Price on request", "Can source", "1-2"}},
		{dealQ, []string{"small chops", "Mama Chops"},
			"Hi 👋 did you buy the [[F1]] from [[F2]]? Reply [[F3]], [[F4]] or [[F5]].",
			[]string{"small chops", "Mama Chops", "YES", "NO", "NOT YET"}},
		{introMsg, nil,
			"Great choice! 🎉\n\n[[F1]]\n[[F2]]\n[[F3]]\n\n[[F4]] pay on delivery or after you see the item. Majo doesn't hold or handle payments.",
			[]string{"Seller: Mike Gadgets", "Chat with them: https://wa.me/2348000000001", "Reference: MM-123456", "Tip:"}},
		{budgetQ, []string{"office chair"},
			"What's your budget for the [[F1]]? A rough range is fine, like [[F2]].",
			[]string{"office chair", "50k-80k"}},
		{searchAk, []string{"laptop"},
			"Thanks! I'm searching for sellers near [[F1]] for your [[F2]] now. I'll message you when someone replies.",
			[]string{"Unilag", "laptop"}},
		{lateMsg, nil,
			"[[F1]] I couldn't find a seller in [[F2]]. Can I search in nearby areas too? Reply [[F3]] or [[F4]].",
			[]string{"Earlier update:", "Yaba", "YES", "NO"}},
		{"Thanks Ada Obi, I got it.", nil, "Thanks [[F1]], I got it.", []string{"Ada Obi"}},
		{"Did Mike Gadgets reply? I think Mike Gadgets is busy.", nil, "Did [[F1]] reply? I think [[F2]] is busy.", []string{"Mike Gadgets", "Mike Gadgets"}},
		{`Send "STOP" to opt out. Order MM-55 costs N15,000 for 3 pcs at 2:30pm.`, nil,
			"Send [[F1]] to opt out. Order [[F2]] costs [[F3]] for [[F4]] at [[F5]].",
			[]string{`"STOP"`, "MM-55", "N15,000", "3 pcs", "2:30pm"}},
	}
	for _, c := range cases {
		m := Mask(c.text, c.keep)
		if m.Text != c.want || fmt.Sprint(m.Facts) != fmt.Sprint(c.facts) {
			t.Errorf("Mask(%q)\n got  %q %q\n want %q %q", c.text, m.Text, m.Facts, c.want, c.facts)
		}
		if got := m.Unmask(m.Text); got != c.text {
			t.Errorf("round trip:\n%q\n%q", got, c.text)
		}
	}
}

// A masked text always unmasks to exactly the original.
func FuzzMaskRoundTrip(f *testing.F) {
	for _, s := range []string{offerMsg, dealQ, introMsg, budgetQ, searchAk, lateMsg, "  Note:  two  spaces\n x", "", "a\r\nb: c"} {
		f.Add(s, "chops")
	}
	f.Fuzz(func(t *testing.T, s, keep string) {
		if strings.Contains(s, "[[F") {
			return // the layer skips such text
		}
		m := Mask(s, []string{keep})
		if got := m.Unmask(m.Text); got != s {
			t.Fatalf("round trip broke:\n%q\n%q\n%q", s, m.Text, got)
		}
	})
}

func TestCheckAcceptsGoodRewrites(t *testing.T) {
	for _, c := range []struct{ text, keep, rewrite string }{
		{dealQ, "small chops|Mama Chops", "How far 👋 you don buy the [[F1]] from [[F2]]? Abeg reply [[F3]], [[F4]] or [[F5]]."},
		{budgetQ, "office chair", "Wetin be your budget for the [[F1]]? Rough range is fine o, like [[F2]]."},
		{searchAk, "laptop", "Thank you! I dey search for sellers near [[F1]] for your [[F2]] now. I go message you once someone replies."},
		{lateMsg, "", "[[F1]] I no fit find seller for [[F2]] o. Make I search for nearby areas too? Reply [[F3]] or [[F4]]."},
		{offerMsg, "iphone 13", "Good news! I found [[F1]] options for [[F2]] 👀\n\n[[F3]]\n   [[F4]]\n   [[F5]]\n\n[[F6]]\n   [[F7]]\n   [[F8]]\n\nReply with the number ([[F9]]) of the one you want."},
		{"Sorry, no seller has replied yet for the gas cylinder. I'm still checking and I'll update you.", "gas cylinder",
			"Sorry o, no seller don reply yet for the [[F1]]. I still dey check, I go update you."},
		{"Okay 👍 I'll check back later.", "", "No wahala 👍 I go check back later."},
	} {
		m := Mask(c.text, strings.Split(c.keep, "|"))
		if r := Check(m, c.rewrite); r != "" {
			t.Errorf("rejected (%s):\n%s\n%s", r, m.Text, c.rewrite)
		}
	}
}

func TestCheckRejects(t *testing.T) {
	m := Mask(dealQ, []string{"small chops", "Mama Chops"})
	good := "How far 👋 you don buy the [[F1]] from [[F2]]? Abeg reply [[F3]], [[F4]] or [[F5]]."
	if Check(m, good) != "" {
		t.Fatal("baseline must pass")
	}
	for want, bad := range map[string]string{
		"empty":             "  ",
		"tokens_changed":    "You don buy the [[F1]] from [[F2]]? Reply [[F4]], [[F3]] or [[F5]].", // swapped YES/NO
		"tokens_changed ":   "You don buy the [[F1]]? Reply [[F3]], [[F4]] or [[F5]].",             // seller dropped
		"tokens_changed  ":  "You don buy the [[F1]] from [[F2]] [[F2]]? Reply [[F3]], [[F4]] or [[F5]].",
		"tokens_changed   ": "You don buy the [[F1]] from [[F2]]? Reply [[F3]], [[F4]] or [[F5]] or [[F6]].",
		"digits_added":      "You don buy the 2 [[F1]] from [[F2]]? Reply [[F3]], [[F4]] or [[F5]].",
		"digits_added ":     "You don buy the [[F1]] from [[F2]] for ₦? Reply [[F3]], [[F4]] or [[F5]].",
		"link_added":        "You don buy the [[F1]] from [[F2]]? Reply [[F3]], [[F4]] or [[F5]] on wa.me/x.",
		"added:tomorrow":    "You don buy the [[F1]] from [[F2]]? Reply [[F3]], [[F4]] or [[F5]] before tomorrow.",
		"added:free":        "You don buy the [[F1]] from [[F2]]? It is free. Reply [[F3]], [[F4]] or [[F5]].",
		"added:delivery":    "You don buy the [[F1]] from [[F2]] with delivery? Reply [[F3]], [[F4]] or [[F5]].",
		"question_lost":     "Tell me if you buy the [[F1]] from [[F2]]. Reply [[F3]], [[F4]] or [[F5]].",
		"negation_changed":  "You no buy the [[F1]] from [[F2]]? Reply [[F3]], [[F4]] or [[F5]].",
		"length":            "Hi you buy the [[F1]] from [[F2]]? Reply [[F3]], [[F4]] or [[F5]]." + strings.Repeat(" please", 30),
	} {
		if got := Check(m, bad); got != strings.TrimSpace(want) {
			t.Errorf("Check(%q) = %q, want %q", bad, got, strings.TrimSpace(want))
		}
	}
	// An item word outside the masked terms must survive.
	m2 := Mask("I'm still looking for a rechargeable fan for you.", nil)
	if got := Check(m2, "I still dey look for a fan for you."); got != "dropped:rechargeable" {
		t.Errorf("got %q", got)
	}
}

type fakeRW struct {
	fn    func(string) string
	err   error
	delay time.Duration
	calls int
	seen  []string
}

func (f *fakeRW) RewriteTone(ctx context.Context, masked string) (string, string, error) {
	f.calls++
	f.seen = append(f.seen, masked)
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	}
	if f.err != nil {
		return "", "", f.err
	}
	return f.fn(masked), "test-model", nil
}

func pidgin(s string) string {
	return strings.Replace(strings.Replace(s, "did you buy", "you don buy", 1), "Reply", "Abeg reply", 1)
}

func TestLayerModes(t *testing.T) {
	ctx := context.Background()
	keep := []string{"small chops", "Mama Chops"}

	rw := &fakeRW{fn: pidgin}
	if out, meta := New(Config{Mode: Off}, rw, nil).Tone(ctx, dealQ, keep); out != dealQ || meta != nil || rw.calls != 0 {
		t.Fatalf("off: no call, no change: %q %v", out, meta)
	}
	if New(Config{Mode: On}, nil, nil).Mode() != Off {
		t.Fatal("no rewriter means off")
	}

	out, meta := New(Config{Mode: Shadow}, rw, nil).Tone(ctx, dealQ, keep)
	want := "Hi 👋 you don buy the small chops from Mama Chops? Abeg reply YES, NO or NOT YET."
	if out != dealQ || meta["result"] != "shadow" || meta["candidate"] != want || meta["model"] != "test-model" {
		t.Fatalf("shadow sends the original, stores the candidate: %q %v", out, meta)
	}
	for _, s := range rw.seen {
		if strings.Contains(s, "Mama") || strings.Contains(s, "chops") || strings.Contains(s, "YES") {
			t.Fatalf("facts reached the model: %q", s)
		}
	}

	out, meta = New(Config{Mode: On}, rw, nil).Tone(ctx, dealQ, keep)
	if out != want || meta["result"] != "applied" || meta["original"] != dealQ {
		t.Fatalf("on: %q %v", out, meta)
	}

	for name, f := range map[string]*fakeRW{
		"rewrite_failed": {err: errors.New("503 unavailable")},
		"rejected":       {fn: func(s string) string { return strings.Replace(s, "[[F3]], [[F4]]", "[[F4]], [[F3]]", 1) }},
	} {
		out, meta = New(Config{Mode: On}, f, nil).Tone(ctx, dealQ, keep)
		if out != dealQ || meta["result"] != name {
			t.Fatalf("%s: original sent: %q %v", name, out, meta)
		}
	}

	slow := &fakeRW{fn: pidgin, delay: time.Second}
	start := time.Now()
	out, meta = New(Config{Mode: On, Timeout: 50 * time.Millisecond}, slow, nil).Tone(ctx, dealQ, keep)
	if out != dealQ || meta["result"] != "rewrite_failed" || time.Since(start) > 500*time.Millisecond {
		t.Fatalf("timeout: original sent quickly: %q %v", out, meta)
	}

	rw.calls = 0
	if out, meta = New(Config{Mode: On, MaxLen: 20}, rw, nil).Tone(ctx, dealQ, keep); out != dealQ || meta["result"] != "skipped_too_long" || rw.calls != 0 {
		t.Fatalf("too long: %v", meta)
	}
	if out, meta = New(Config{Mode: On}, rw, nil).Tone(ctx, "1. A\n2. B", nil); out != "1. A\n2. B" || meta["result"] != "skipped_no_prose" || rw.calls != 0 {
		t.Fatalf("nothing to rewrite: %v", meta)
	}
	if out, meta = New(Config{Mode: On}, rw, nil).Tone(ctx, "Reply [[F1]] please now", nil); meta["result"] != "skipped_token_like_text" || out != "Reply [[F1]] please now" {
		t.Fatalf("token-like text: %v", meta)
	}
}

// Whatever the model answers, every fact reaches the buyer unchanged and
// in order: either the rewrite passed the checks, or the original is sent.
func TestFactsNeverChange(t *testing.T) {
	ctx := context.Background()
	rnd := rand.New(rand.NewSource(7))
	tok := regexp.MustCompile(`\[\[F\d+\]\]`)
	mutations := []func(string) string{
		func(s string) string { return s },
		pidgin,
		func(s string) string { // swap two tokens
			ts := tok.FindAllString(s, -1)
			if len(ts) < 2 {
				return s
			}
			i, j := rnd.Intn(len(ts)), rnd.Intn(len(ts))
			return strings.Replace(strings.Replace(strings.Replace(s, ts[i], "\x00", 1), ts[j], ts[i], 1), "\x00", ts[j], 1)
		},
		func(s string) string {
			return tok.ReplaceAllStringFunc(s, func(t string) string { return t[:len(t)-3] + "9]]" })
		},
		func(s string) string {
			ts := tok.FindAllString(s, -1)
			return strings.Replace(s, ts[rnd.Intn(len(ts))], "", 1)
		},
		func(s string) string { return s + " Only 5 left!" },
		func(s string) string { return strings.Replace(s, " the ", " the cheap ", 1) },
		func(s string) string { return strings.Replace(s, " ", " "+fmt.Sprint(rnd.Intn(100))+" ", 1) },
		func(s string) string { return strings.ReplaceAll(s, "[[", "[") },
		func(s string) string {
			words := strings.Fields(s)
			rnd.Shuffle(len(words), func(i, j int) { words[i], words[j] = words[j], words[i] })
			return strings.Join(words, " ")
		},
		func(s string) string { return "" },
	}
	inputs := []struct {
		text string
		keep []string
	}{{offerMsg, []string{"iphone 13"}}, {dealQ, []string{"small chops", "Mama Chops"}}, {introMsg, nil},
		{budgetQ, []string{"office chair"}}, {searchAk, []string{"laptop"}}, {lateMsg, nil}}
	for i := 0; i < 3000; i++ {
		in := inputs[rnd.Intn(len(inputs))]
		mut := mutations[rnd.Intn(len(mutations))]
		out, _ := New(Config{Mode: On}, &fakeRW{fn: mut}, nil).Tone(ctx, in.text, in.keep)
		if out == in.text {
			continue
		}
		facts := Mask(in.text, in.keep).Facts
		rest := out
		for _, f := range facts {
			k := strings.Index(rest, f)
			if k < 0 {
				t.Fatalf("fact %q lost or reordered:\n%s\n->\n%s", f, in.text, out)
			}
			rest = rest[k+len(f):]
		}
		if regexp.MustCompile(`\d`).MatchString(tok.ReplaceAllString(Mask(out, in.keep).Text, "")) {
			t.Fatalf("a number outside the facts:\n%s", out)
		}
	}
}

func TestParseMode(t *testing.T) {
	for in, want := range map[string]Mode{"": Off, "off": Off, "OFF": Off, "shadow": Shadow, "on": On, " On ": On} {
		if got, err := ParseMode(in); err != nil || got != want {
			t.Fatalf("ParseMode(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseMode("loud"); err == nil {
		t.Fatal("unknown mode refused")
	}
}

// Every real buyer message: the round trip is exact, no digit, link or
// reply keyword is left for the model to see, and there is prose to rewrite.
func TestSamplesMaskEveryFact(t *testing.T) {
	digit := regexp.MustCompile(`\d|₦|https?://|wa\.me|\b(YES|NO|NOT YET)\b`)
	for _, s := range Samples {
		m := Mask(s.Text, s.Keep)
		if m.Unmask(m.Text) != s.Text {
			t.Errorf("%s: round trip", s.Name)
		}
		if left := digit.FindString(tokenRe.ReplaceAllString(m.Text, "")); left != "" {
			t.Errorf("%s: %q visible to the model in %q", s.Name, left, m.Text)
		}
		for _, k := range s.Keep {
			if strings.Contains(m.Text, k) {
				t.Errorf("%s: keep term %q visible in %q", s.Name, k, m.Text)
			}
		}
		if !m.HasProse() {
			t.Errorf("%s: nothing to rewrite in %q", s.Name, m.Text)
		}
		if Check(m, m.Text) != "" {
			t.Errorf("%s: the unchanged text must pass its own check: %s", s.Name, Check(m, m.Text))
		}
	}
}
