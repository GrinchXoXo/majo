// Package workflow is Majo's autonomous process manager (Phase 4C
// correction). It turns each buyer request into a sequence of typed
// decisions — confirm, clarify, route, answer — validated by deterministic
// policy and executed by the existing application services:
//
//	message -> context builder -> interpreter -> typed proposal
//	        -> deterministic policy -> application service -> DB -> event -> outbound
//
// No human gate exists on the normal path. Operators observe, pause,
// override and handle the exceptions this package raises; they are never
// required to advance a healthy request. Interpretation is deterministic
// where possible; AI is used only through the existing extraction service
// (it proposes field values; it never writes state).
package workflow

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/request"
)

// Fields Majo may ask a buyer about, in the order it asks.
const (
	FieldProduct   = "product"
	FieldLocation  = "location"
	FieldSpec      = "spec"
	FieldBudget    = "budget"
	FieldCondition = "condition"
)

// genericProducts are products too broad to route well without one more
// detail; the value is the question for that detail. Deliberately small and
// explicit (no taxonomy service).
var genericProducts = map[string]string{
	"tv":         "What size are you looking for, roughly?",
	"television": "What size are you looking for, roughly?",
	"phone":      "Which phone model are you looking for?",
	"smartphone": "Which phone model are you looking for?",
	"iphone":     "Which iPhone model are you looking for?",
	"samsung":    "Which Samsung model are you looking for?",
	"laptop":     "Which laptop model (or specs) are you looking for?",
	"tecno":      "Which Tecno model are you looking for?",
	"infinix":    "Which Infinix model are you looking for?",
}

// History is what Majo already asked about this request.
type History struct {
	Asked   map[string]int  // field -> times asked
	Skipped map[string]bool // the buyer said it doesn't matter / couldn't say
	Total   int             // all questions asked for this request
}

// HistoryFrom summarises the clarifications of one request.
func HistoryFrom(cs []*autonomy.Clarification) History {
	h := History{Asked: map[string]int{}, Skipped: map[string]bool{}}
	for _, c := range cs {
		if c.Party != autonomy.PartyBuyer || c.Kind != autonomy.KindMissingField || c.Status == autonomy.ClarCancelled || c.Status == autonomy.ClarSuperseded {
			continue // never delivered, or interrupted before the buyer answered: ask again
		}
		h.Total += c.Attempts
		h.Asked[c.Field] += c.Attempts
		if skipped, _ := c.Resolution["skipped"].(bool); skipped {
			h.Skipped[c.Field] = true
		}
		if c.Status == autonomy.ClarExhausted {
			h.Skipped[c.Field] = true
		}
	}
	return h
}

// Assessment is the deterministic confirmation decision for one request.
type Assessment struct {
	Clear    bool
	Missing  string // the one field to ask about next ("" when clear)
	Question string
	// FillLocation is a location known from context (consent record or an
	// earlier request) to use instead of asking.
	FillLocation string
	// FillVariant is a model/size the buyer did state but extraction missed
	// ("Samsung A54", "55 inch TV"): used instead of asking again.
	FillVariant string
	Reason      string
	// Options are the choices offered with the question (several products
	// named, or two places offered).
	Options []string
	// Hold: the request names a restricted item — never confirmed or routed
	// automatically; held for operator review (SAFETY_EXCEPTION).
	Hold bool
	// Outcome is the conceptual class of the decision (see Outcome* below).
	Outcome string
}

// Request-understanding outcomes (Phase 4C hardening).
const (
	OutcomeClear       = "clear"            // proceed: confirm and route
	OutcomeMissing     = "missing_detail"   // one material detail missing: ask it
	OutcomeAmbiguous   = "ambiguous"        // several readings: ask which (context did not settle it)
	OutcomeConflicting = "conflicting"      // contradictory details: ask the buyer to resolve
	OutcomeUnsupported = "unanswerable"     // cannot be understood after asking: say so, offer a next step
	OutcomePolicy      = "policy_exception" // restricted item: held for review
)

// Assess decides whether a request is understood well enough to proceed,
// and if not, the single most useful question to ask next. Majo asks only
// what the next action needs: a product (always), a location (unless known
// from context), and for generic products one detail and a budget.
//
// The 0.45 confidence floor (AUTO_CONFIRM_MIN_CONFIDENCE) is NOT the policy:
// with the heuristic extractor 0.45 is exactly the weight of "a product word
// was found", so it acts as a product-present check. The decision is made by
// the deterministic checks below (restricted items, product, several
// products, contradictions, location, generic-product detail, budget).
func Assess(r *request.BuyerRequest, knownLocation string, h History, p autonomy.Params) Assessment {
	a := assess(r, knownLocation, h, p)
	switch {
	case a.Hold:
		a.Outcome = OutcomePolicy
	case a.Clear:
		a.Outcome = OutcomeClear
	case a.Missing == "":
		a.Outcome = OutcomeUnsupported
	case strings.HasPrefix(a.Reason, "several_") || strings.HasPrefix(a.Reason, "location_alternatives"):
		a.Outcome = OutcomeAmbiguous
	case strings.HasSuffix(a.Reason, "_conflict"):
		a.Outcome = OutcomeConflicting
	default:
		a.Outcome = OutcomeMissing
	}
	return a
}

func assess(r *request.BuyerRequest, knownLocation string, h History, p autonomy.Params) Assessment {
	product := strings.TrimSpace(deref(r.Product))
	if term := RestrictedTerm(r.RawMessage+" "+product, p.RestrictedTerms); term != "" {
		return Assessment{Hold: true, Reason: "restricted_item:" + term}
	}
	if r.ListingID != nil && product != "" {
		// Listing-origin demand names one curated listing: nothing to ask.
		return Assessment{Clear: true, Reason: "listing_origin"}
	}
	if product == "" || r.Confidence < p.ConfirmMinConfidence {
		if h.Skipped[FieldProduct] {
			return Assessment{Reason: "product_unknown_after_clarification"}
		}
		return Assessment{Missing: FieldProduct, Question: productQuestion(r), Reason: "product_unknown"}
	}
	signals := signalsOf(r)
	// Several products in one message: ask which to start with (never
	// silently drop one). Asked once.
	if others := signals.otherProducts; len(others) > 0 && h.Asked[FieldProduct] == 0 && !h.Skipped[FieldProduct] {
		opts := append([]string{product}, others...)
		return Assessment{Missing: FieldProduct, Options: opts, Reason: "several_products",
			Question: fmt.Sprintf("I can look for %s. Which should I start with? (I'll take the next one after)", joinOr(opts))}
	}
	// Contradictory condition ("brand new fairly used"): ask, do not guess.
	if signals.has("condition_conflict") && h.Asked[FieldCondition] == 0 && !h.Skipped[FieldCondition] {
		return Assessment{Missing: FieldCondition, Options: []string{"Brand new", "Fairly used"}, Reason: "condition_conflict",
			Question: "Do you want it brand new or fairly used? (Reply \"either\" if both are fine)"}
	}
	// Two places offered as options ("Lagos or Abuja"): ask where to start.
	if signals.has("location_alternatives") && h.Asked[FieldLocation] == 0 && !h.Skipped[FieldLocation] {
		return Assessment{Missing: FieldLocation, Reason: "location_alternatives",
			Question: "Which city should I search in first?"}
	}
	a := Assessment{Clear: true, Reason: "clear"}
	if strings.TrimSpace(deref(r.Location)) == "" && !h.Skipped[FieldLocation] {
		if knownLocation != "" {
			a.FillLocation = knownLocation
			a.Reason = "clear_location_from_context"
		} else {
			return Assessment{Missing: FieldLocation, Question: "Which city or area should I search in?", Reason: "location_unknown"}
		}
	}
	if q, generic := genericProducts[normalizeProduct(product)]; generic {
		if strings.TrimSpace(deref(r.Variant)) == "" && !h.Skipped[FieldSpec] && h.Asked[FieldSpec] == 0 {
			if v := SpecInText(r.RawMessage, product); v != "" {
				a.FillVariant = v
				a.Reason = "clear_spec_from_message"
				return a
			}
			return Assessment{Missing: FieldSpec, Question: q, Reason: "generic_product_needs_detail"}
		}
		// A budget is asked only when the request started vague (Majo had to
		// ask for the model/size): then it narrows a wide price range.
		if h.Asked[FieldSpec] > 0 && r.BudgetMin == nil && r.BudgetMax == nil && !h.Skipped[FieldBudget] && h.Asked[FieldBudget] == 0 {
			return Assessment{Missing: FieldBudget, Question: "And what's your budget?", Reason: "generic_product_needs_budget"}
		}
	}
	return a
}

func productQuestion(r *request.BuyerRequest) string {
	if strings.TrimSpace(r.RawMessage) == "" {
		return "What are you looking for? Tell me the item and I'll search 👀"
	}
	return "Sorry, could you tell me a bit more about what you're looking for?"
}

func normalizeProduct(s string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(s))), " ")
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// skipPhrases mean "it doesn't matter / I can't say": Majo stops asking about
// that field and proceeds.
var skipPhrases = map[string]bool{
	"any": true, "anything": true, "any one": true, "anyone": true, "anyhow": true, "doesn't matter": true, "does not matter": true,
	"dont matter": true, "don't matter": true, "no budget": true, "not sure": true, "i don't know": true, "i dont know": true,
	"no idea": true, "any size": true, "any model": true, "anywhere": true, "any where": true, "any location": true, "whatever": true,
	"open": true, "flexible": true, "no preference": true, "skip": true, "any price": true, "e no matter": true, "anything dey": true,
}

// IsSkip reports whether an answer declines to specify the field.
func IsSkip(text string) bool { return skipPhrases[autonomy.Normalize(text)] }

var yesWords = map[string]bool{"yes": true, "yeah": true, "yea": true, "yep": true, "yup": true, "ok": true, "okay": true, "sure": true,
	"go ahead": true, "please do": true, "do it": true, "oya": true, "alright": true, "check": true, "abeg check": true, "yes please": true,
	"yes pls": true, "please": true, "pls": true, "go on": true, "sharp": true, "correct": true, "i want it": true, "i'll take it": true}

// strongYes is the only "yes" that may select an offer (a commercial
// commitment): an acknowledgement like "ok" never does.
var strongYes = map[string]bool{"yes": true, "yes please": true, "yes pls": true, "yeah": true, "yep": true, "yup": true,
	"i want it": true, "i'll take it": true, "i will take it": true, "go ahead": true}

// StrongYes reports an explicit yes (selection confirmation).
func StrongYes(text string) bool { return strongYes[autonomy.Normalize(text)] }

var noWords = map[string]bool{"no": true, "nope": true, "nah": true, "no thanks": true, "no thank you": true, "dont": true, "don't": true,
	"leave it": true, "not now": true, "no need": true, "cancel": true, "never mind": true, "nevermind": true, "abeg leave am": true}

// YesNo classifies a short answer: +1 yes, -1 no, 0 neither.
func YesNo(text string) int {
	t := autonomy.Normalize(text)
	switch {
	case yesWords[t]:
		return 1
	case noWords[t]:
		return -1
	}
	return 0
}

var (
	sizePattern  = regexp.MustCompile(`(?i)\b(\d{2,3})\s*(?:-\s*)?(inch|inches|in\b|")`)
	modelPattern = regexp.MustCompile(`(?i)^\s*([a-z0-9+-]*\d[a-z0-9+-]*(?:\s+(?:pro|max|plus|ultra|mini|lite|air))*)\b`)
)

// SpecInText finds a model or size the buyer stated right after the product
// word ("samsung a54", "iphone 15 pro") or anywhere as a size ("55 inch").
func SpecInText(raw, product string) string {
	if m := sizePattern.FindStringSubmatch(raw); m != nil {
		return m[1] + " inch"
	}
	low := strings.ToLower(raw)
	i := strings.Index(low, strings.ToLower(product))
	if i < 0 {
		return ""
	}
	if m := modelPattern.FindStringSubmatch(raw[i+len(product):]); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// ---- interpretation signals and policy helpers (Phase 4C hardening) ---------------

type signals struct {
	otherProducts []string
	ambiguities   map[string]bool
}

func (s signals) has(k string) bool { return s.ambiguities[k] }

// signalsOf reads what the interpreter reported in the request's original
// extraction output (extracted_data). An extractor that reports nothing
// leaves the request as before.
func signalsOf(r *request.BuyerRequest) signals {
	s := signals{ambiguities: map[string]bool{}}
	if r.ExtractedData == nil {
		return s
	}
	if v, ok := r.ExtractedData["other_products"].([]any); ok {
		for _, x := range v {
			if p, ok := x.(string); ok && strings.TrimSpace(p) != "" {
				s.otherProducts = append(s.otherProducts, strings.TrimSpace(p))
			}
		}
	}
	if v, ok := r.ExtractedData["ambiguities"].([]any); ok {
		for _, x := range v {
			if a, ok := x.(string); ok {
				s.ambiguities[a] = true
			}
		}
	}
	return s
}

func joinOr(xs []string) string {
	switch len(xs) {
	case 0:
		return ""
	case 1:
		return xs[0]
	}
	return strings.Join(xs[:len(xs)-1], ", ") + " or " + xs[len(xs)-1]
}

var nonWord = regexp.MustCompile(`[^a-z0-9-]+`)

// RestrictedTerm returns the first restricted term the text names as whole
// word(s) ("" when none). Matching is deliberately literal: the list is a
// safety net for automatic routing, not content moderation.
func RestrictedTerm(text string, terms []string) string {
	t := " " + strings.TrimSpace(nonWord.ReplaceAllString(strings.ToLower(text), " ")) + " "
	for _, term := range terms {
		norm := strings.TrimSpace(nonWord.ReplaceAllString(strings.ToLower(term), " "))
		if norm != "" && strings.Contains(t, " "+norm+" ") {
			return term
		}
	}
	return ""
}

// ---- conversational small talk (no request) ----------------------------------------

// acknowledgements are courtesy messages that ask for nothing. Affirmatives
// that may mean "I choose it" ("ok", "yes", "that one") are NOT here: the
// offer resolver answers those with a clarification (Phase 3.1).
var acknowledgements = map[string]bool{"k": true, "kk": true, "okk": true, "oki": true,
	"thanks": true, "thank you": true, "thank u": true, "thanks a lot": true, "thanks o": true, "tnx": true, "thx": true, "tanx": true,
	"alright o": true, "noted": true, "cool": true, "fine": true, "great": true, "nice": true,
	"ok thanks": true, "okay thanks": true, "ok thank you": true, "okay thank you": true, "no problem": true, "np": true,
	"no": true, "nope": true, "thank you so much": true, "i see": true, "ehen": true, "gracias": true, "e don do": true, "seen": true,
	"thanks so much": true, "thank you very much": true, "much appreciated": true, "okay noted": true, "ok noted": true}

var greetings = map[string]bool{"hi": true, "hello": true, "hey": true, "hi there": true, "hello there": true, "good morning": true,
	"good afternoon": true, "good evening": true, "morning": true, "how far": true, "howfa": true, "how you dey": true, "hello majo": true,
	"hi majo": true, "hey majo": true, "good day": true, "hiya": true, "hy": true, "helo": true, "hallo": true, "sup": true, "wassup": true,
	"whats up": true, "what's up": true, "how are you": true, "how are you doing": true, "good morning sir": true, "good morning ma": true}

// SmallTalk classifies a whole message as "ack" (acknowledgement / bare
// yes-no with nothing open), "greeting", or "" (anything else). Only exact
// whole-message matches count: "hello, I need a fridge" is a request.
func SmallTalk(text string) string {
	raw := strings.TrimSpace(text)
	t := autonomy.Normalize(strings.Trim(raw, ".,!?🙏👍😊🙂👌❤️ "))
	t = strings.TrimSuffix(strings.TrimSuffix(t, " sir"), " ma")
	switch {
	case raw == "":
		return ""
	case t == "": // only emoji / punctuation ("👍", "🙏")
		return "ack"
	case acknowledgements[t] || acknowledgements[autonomy.Normalize(text)]:
		return "ack"
	case greetings[t] || greetings[autonomy.Normalize(text)]:
		return "greeting"
	}
	return smallTalkClauses(raw)
}

// Pilot batch: Nigerian small talk rarely arrives as one bare phrase. "good
// morning o, how una dey", "thank you jare 🙏", "lol" and "una dey there?"
// each opened a BuyerRequest and got "what are you looking for?". A message
// is small talk when EVERY clause is a greeting, a courtesy, a presence check
// or laughter once Pidgin particles and emoji are removed. Any other word
// ("good morning, I need a fridge") still makes it a request.
var presenceChecks = map[string]bool{"una dey there": true, "you dey there": true, "are you there": true, "anybody there": true,
	"is anyone there": true, "how una dey": true, "how body": true, "how una": true, "how your side": true, "hope you dey fine": true,
	"hope una dey fine": true, "how is it going": true, "how you doing": true, "how u dey": true, "hw far": true, "how far na": true}
var laughter = map[string]bool{"lol": true, "lmao": true, "lmfao": true, "haha": true, "hahaha": true, "hehe": true, "rofl": true}
var trailingParticles = []string{" ooo", " oo", " o", " jare", " sef", " abeg", " please", " pls", " biko", " my guy", " boss",
	" bro", " guys", " una", " sir", " ma", " dear"}

func smallTalkClauses(raw string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(raw) {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '\'' || r == ' ':
			b.WriteRune(r)
		case r == ',' || r == '.' || r == '!' || r == '?' || r == ';' || r == '\n':
			b.WriteRune('|')
		default:
			b.WriteRune(' ')
		}
	}
	kind, seen := "ack", false
	for _, clause := range strings.Split(b.String(), "|") {
		c := strings.Join(strings.Fields(clause), " ")
		if c == "" {
			continue
		}
		for changed := true; changed; {
			changed = false
			for _, p := range trailingParticles {
				if strings.HasSuffix(c, p) && len(c) > len(p) {
					c, changed = strings.TrimSuffix(c, p), true
				}
			}
		}
		seen = true
		switch {
		case greetings[c] || presenceChecks[c]:
			kind = "greeting"
		case acknowledgements[c] || laughter[c]:
		default:
			return ""
		}
	}
	if !seen {
		if strings.TrimSpace(raw) == "" {
			return ""
		}
		return "ack" // only emoji / punctuation ("😂😂", "🙏🏾")
	}
	return kind
}
