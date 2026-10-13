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
	"regexp"
	"strings"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/request"
)

// Fields Majo may ask a buyer about, in the order it asks.
const (
	FieldProduct  = "product"
	FieldLocation = "location"
	FieldSpec     = "spec"
	FieldBudget   = "budget"
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
}

// Assess decides whether a request is understood well enough to proceed,
// and if not, the single most useful question to ask next. Majo asks only
// what the next action needs: a product (always), a location (unless known
// from context), and for generic products one detail and a budget.
func Assess(r *request.BuyerRequest, knownLocation string, h History, p autonomy.Params) Assessment {
	product := strings.TrimSpace(deref(r.Product))
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
	modelPattern = regexp.MustCompile(`(?i)^\s*([a-z0-9][a-z0-9+-]*\d[a-z0-9+-]*(?:\s+(?:pro|max|plus|ultra|mini|lite|air))?)`)
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
