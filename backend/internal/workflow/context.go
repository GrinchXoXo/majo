package workflow

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/routing"
)

// Context item kinds.
const (
	ItemOffer   = "offer"   // an option presented to the buyer ("1.", "2.")
	ItemListing = "listing" // a listing shown in a discovery delivery
	ItemRequest = "request" // something the buyer asked for earlier
)

// ContextItem is one thing the buyer may be referring to. The context
// builder reads it from persisted state (offers, deliveries, listings,
// requests): no separate conversation state can drift from what was sent.
type ContextItem struct {
	Kind           string     `json:"kind"`
	Group          string     `json:"group"` // items shown together (one offer message / one delivery)
	At             time.Time  `json:"at"`
	Ordinal        int        `json:"ordinal,omitempty"`
	OfferID        *uuid.UUID `json:"offer_id,omitempty"`
	OfferStatus    string     `json:"offer_status,omitempty"`
	ListingID      *uuid.UUID `json:"listing_id,omitempty"`
	DeliveryID     *uuid.UUID `json:"delivery_id,omitempty"`
	BuyerRequestID *uuid.UUID `json:"buyer_request_id,omitempty"`
	RequestStatus  string     `json:"request_status,omitempty"`
	Ref            string     `json:"ref,omitempty"`
	Category       string     `json:"category,omitempty"`
	Product        string     `json:"product"`
	Variant        string     `json:"variant,omitempty"`
	Condition      string     `json:"condition,omitempty"`
	Description    string     `json:"description,omitempty"`
	Location       string     `json:"location,omitempty"`
	Price          *int64     `json:"price,omitempty"`
	Focus          bool       `json:"focus,omitempty"` // the listing the buyer last interacted with
}

// Label is how Majo names the item back to the buyer.
func (i ContextItem) Label() string {
	l := strings.TrimSpace(i.Product + " " + i.Variant)
	if l == "" {
		l = "that item"
	}
	return l
}

func (i ContextItem) text() string {
	return strings.Join([]string{i.Product, i.Variant, i.Condition, i.Description, i.Category, i.Ref}, " ")
}

// BuyerContext is the relevant context for one buyer message.
type BuyerContext struct {
	ConversationID uuid.UUID
	BuyerID        uuid.UUID
	KnownLocation  string
	Items          []ContextItem // newest first
}

// ---- referential intent ----------------------------------------------------------

// Reference intents.
const (
	IntentNone         = ""
	IntentPrice        = "price"        // "how much is the black one?"
	IntentAvailability = "availability" // "is it still available?"
	IntentWant         = "want"         // "I want that one", "I need that phone"
	IntentCheaper      = "cheaper"      // "find me something cheaper"
)

// Reference is the deterministic reading of a referential message.
type Reference struct {
	Intent string
	Noun   string          // "phone", "tv", ... ("" = any)
	Attrs  map[string]bool // identifying words ("black", "128gb")
	Amount *int64          // "the 120k one"
}

var (
	cheaperPattern = regexp.MustCompile(`(?i)\b(cheaper|less expensive|lower price|cheapest|reduce(d)? price|something less)\b`)
	pricePattern   = regexp.MustCompile(`(?i)\b(how much|wetin be the price|what'?s the price|what is the price|price of|the price|how much be)\b`)
	availPattern   = regexp.MustCompile(`(?i)\b(still available|is it available|available\?|e still dey|still dey|dey available|in stock)\b`)
	wantPattern    = regexp.MustCompile(`(?i)\b(i want|i need|i('ll| will) take|give me|send me|i go take|make i get|i'?d like|i like)\b`)
	markerPattern  = regexp.MustCompile(`(?i)\b(that|this|those|these|it|am)\b|\bthe\s+\w+(\s+\w+)?\s+one\b|\bthe\s+(first|second|third|last|other)\b`)
	amountPattern  = regexp.MustCompile(`(?i)\b(\d+(?:\.\d+)?)\s*k\b|₦\s*([\d,]{4,})`)
	nouns          = []string{"phone", "iphone", "laptop", "tv", "television", "fridge", "shoe", "shoes", "bag", "watch", "console", "ps5", "item", "thing", "product"}
)

// intentWords never identify an item.
var intentWords = map[string]bool{"how": true, "much": true, "price": true, "still": true, "available": true, "cheaper": true,
	"something": true, "find": true, "show": true, "wetin": true, "be": true, "what": true, "s": true, "whats": true, "like": true,
	"take": true, "give": true, "send": true, "go": true, "make": true, "ll": true, "will": true, "d": true, "would": true, "can": true,
	"stock": true, "first": true, "second": true, "third": true, "last": true, "other": true, "those": true, "these": true,
	"k": true, "naira": true, "less": true, "expensive": true, "lower": true, "cheapest": true, "dey": true, "e": true, "there": true,
	// conversational fillers
	"now": true, "again": true, "then": true, "today": true, "here": true, "oh": true, "o": true, "sir": true, "ma": true, "bro": true,
	"boss": true, "biko": true, "abeg": true, "pls": true, "please": true, "thanks": true, "thank": true, "ehn": true, "so": true}

// ParseReference recognises a message that refers to something already in
// the conversation. IntentNone means "not referential": the message is an
// ordinary request (or selection) and continues down the normal path.
func ParseReference(text string) Reference {
	t := strings.ToLower(strings.TrimSpace(text))
	ref := Reference{Attrs: map[string]bool{}}
	switch {
	case cheaperPattern.MatchString(t):
		ref.Intent = IntentCheaper
	case pricePattern.MatchString(t) && markerPattern.MatchString(t):
		ref.Intent = IntentPrice
	case availPattern.MatchString(t) && markerPattern.MatchString(t):
		ref.Intent = IntentAvailability
	case wantPattern.MatchString(t) && markerPattern.MatchString(t):
		ref.Intent = IntentWant
	default:
		return Reference{}
	}
	if m := amountPattern.FindStringSubmatch(t); m != nil {
		if m[1] != "" {
			if f, err := strconv.ParseFloat(m[1], 64); err == nil {
				v := int64(f * 1000)
				ref.Amount = &v
			}
		} else if n, err := strconv.ParseInt(strings.ReplaceAll(m[2], ",", ""), 10, 64); err == nil {
			ref.Amount = &n
		}
	}
	for tok := range routing.Tokens(t) {
		if intentWords[tok] || isNumberish(tok) {
			continue
		}
		isNoun := false
		for _, n := range nouns {
			if tok == n {
				if ref.Noun == "" && tok != "item" && tok != "thing" && tok != "product" {
					ref.Noun = tok
				}
				isNoun = true
			}
		}
		if !isNoun {
			ref.Attrs[tok] = true
		}
	}
	return ref
}

// isNumberish: an amount ("120k", "650000"), not a model number ("13").
func isNumberish(t string) bool {
	if strings.HasSuffix(t, "k") {
		_, err := strconv.ParseFloat(strings.TrimSuffix(t, "k"), 64)
		return err == nil
	}
	n, err := strconv.ParseInt(t, 10, 64)
	return err == nil && n >= 1000
}

// nounMatches: does the item fit the noun the buyer used ("that phone")?
func nounMatches(noun string, it ContextItem) bool {
	if noun == "" {
		return true
	}
	t := strings.ToLower(it.text())
	switch noun {
	case "phone":
		for _, w := range []string{"phone", "iphone", "samsung", "galaxy", "tecno", "infinix", "pixel", "redmi", "itel", "nokia"} {
			if strings.Contains(t, w) {
				return true
			}
		}
		return false
	case "tv", "television":
		return strings.Contains(t, "tv") || strings.Contains(t, "television")
	case "console", "ps5":
		return strings.Contains(t, "ps5") || strings.Contains(t, "playstation") || strings.Contains(t, "xbox") || strings.Contains(t, "console")
	}
	return strings.Contains(t, noun)
}

// ResolveReference returns the context items the reference fits, most
// specific first. One item = resolved; several = ask which; none = not a
// reference to anything Majo showed (normal path).
func ResolveReference(ref Reference, items []ContextItem) []ContextItem {
	if ref.Intent == IntentNone || len(items) == 0 {
		return nil
	}
	var pool []ContextItem
	for _, it := range items {
		if ref.Intent == IntentCheaper && it.Price == nil && it.Kind != ItemRequest {
			continue
		}
		if ref.Intent == IntentPrice && it.Kind == ItemRequest {
			continue // Majo has no price for something the buyer asked for
		}
		if nounMatches(ref.Noun, it) {
			pool = append(pool, it)
		}
	}
	if len(pool) == 0 {
		return nil
	}
	if ref.Amount != nil {
		var hit []ContextItem
		for _, it := range pool {
			if it.Price != nil && within(*it.Price, *ref.Amount, 0.03) {
				hit = append(hit, it)
			}
		}
		if len(hit) > 0 {
			return hit
		}
	}
	if len(ref.Attrs) > 0 {
		// Every identifying word the buyer used must describe the item:
		// "the black one" fits a black phone; "an iPhone 14", "a phone that
		// has good battery" or "256gb iPhone 13" do not fit an iPhone 12 —
		// those are new requests, never a reference to something else.
		var hit []ContextItem
		for _, it := range pool {
			if routing.TokenOverlap(ref.Attrs, it.text()) == len(ref.Attrs) {
				hit = append(hit, it)
			}
		}
		return dedupe(hit)
	}
	// "it" / "that one" / "that phone": the item in focus, else the latest
	// group shown (offers or a delivery), else everything that fits.
	for _, it := range pool {
		if it.Focus {
			return []ContextItem{it}
		}
	}
	latest := pool[0].Group
	var group []ContextItem
	for _, it := range pool {
		if it.Group == latest {
			group = append(group, it)
		}
	}
	return dedupe(group)
}

func within(a, b int64, tol float64) bool {
	d := float64(a - b)
	if d < 0 {
		d = -d
	}
	return d <= tol*float64(b)
}

func dedupe(items []ContextItem) []ContextItem {
	seen := map[string]bool{}
	var out []ContextItem
	for _, it := range items {
		k := it.Kind + ":" + it.Ref + ":" + strconv.Itoa(it.Ordinal) + ":" + it.Product + ":" + it.Group
		if !seen[k] {
			seen[k] = true
			out = append(out, it)
		}
	}
	return out
}

// WhichOptions turns ambiguous items into a short numbered question.
func WhichOptions(items []ContextItem, intent string) (string, []autonomy.Option) {
	if len(items) > 4 {
		items = items[:4]
	}
	sort.SliceStable(items, func(i, j int) bool { return priceOf(items[i]) < priceOf(items[j]) })
	var opts []autonomy.Option
	var parts []string
	for i, it := range items {
		key := strconv.Itoa(i + 1)
		label := it.Label()
		if it.Price != nil {
			label += " at ₦" + thousands(*it.Price)
		}
		parts = append(parts, fmt.Sprintf("%s. %s", key, label))
		opts = append(opts, autonomy.Option{Key: key, Label: label, Ref: it.Ref, Value: map[string]any{"item": it}})
	}
	q := "Sure — which one do you mean?\n" + strings.Join(parts, "\n") + "\n\nReply with the number."
	if len(items) == 2 && items[0].Price != nil && items[1].Price != nil {
		q = fmt.Sprintf("Do you mean the ₦%s one or the ₦%s one?\n1. %s\n2. %s\n\nReply 1 or 2.",
			thousands(*items[0].Price), thousands(*items[1].Price), opts[0].Label, opts[1].Label)
	}
	return q, opts
}

func priceOf(it ContextItem) int64 {
	if it.Price == nil {
		return 1 << 62
	}
	return *it.Price
}

func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	return string(out)
}

// MatchItemOption maps an answer to "which one?" onto one option: its
// number, a price ("the 120k one") or the only label it names.
func MatchItemOption(text string, opts []autonomy.Option) *autonomy.Option {
	if o := routing.MatchVendorOption(text, opts); o != nil {
		return o
	}
	if m := amountPattern.FindStringSubmatch(strings.ToLower(text)); m != nil {
		var amt int64
		if m[1] != "" {
			f, _ := strconv.ParseFloat(m[1], 64)
			amt = int64(f * 1000)
		} else {
			amt, _ = strconv.ParseInt(strings.ReplaceAll(m[2], ",", ""), 10, 64)
		}
		var hit *autonomy.Option
		for i := range opts {
			if strings.Contains(opts[i].Label, "₦"+thousands(amt)) {
				if hit != nil {
					return nil
				}
				hit = &opts[i]
			}
		}
		return hit
	}
	return nil
}
