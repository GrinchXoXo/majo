// Package matching implements deterministic, explainable vendor scoring
// against a confirmed BuyerRequest. Per PRD 2 §16 and Middleman_Technical_
// Spec_v1 §4: rule-based only, no ML/embeddings, weights configurable.
package matching

import (
	"sort"
	"strings"
	"unicode"

	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/vendor"
)

// Weights are the v1 default point allocations from PRD 2 §18. Explicitly
// not permanent — passed as a parameter so they can be tuned without
// touching scoring logic.
type Weights struct {
	CategoryMax     int
	ProductMax      int
	LocationMax     int
	ConditionMax    int
	AvailabilityMax int
}

func DefaultWeights() Weights {
	return Weights{
		CategoryMax:     30,
		ProductMax:      30,
		LocationMax:     20,
		ConditionMax:    10,
		AvailabilityMax: 10,
	}
}

const (
	DefaultMinRelevanceScore = 40 // PRD 2 §25
	DefaultMaxCandidates     = 5  // PRD 2 §21/§26
)

type MatchType string

const (
	MatchSupplyConfirmed MatchType = "SUPPLY_CONFIRMED"
	MatchProductCapable  MatchType = "PRODUCT_CAPABLE"
	MatchCanSource       MatchType = "CAN_SOURCE"
	MatchCategoryOnly    MatchType = "CATEGORY_ONLY"
)

// Candidate is a single scored vendor, with enough detail for an operator
// to understand why it appeared (PRD 2 §28).
type Candidate struct {
	Vendor         *vendor.Vendor
	Score          int
	MatchType      MatchType
	Breakdown      map[string]int
	Reasons        []string
	RelevantSupply *supply.Signal
	// RelevantListing is set when the strongest product evidence is an
	// operator-published (ACTIVE) SupplyListing (Phase 4A, Lock V2 §M-4A.6).
	// At most one of RelevantSupply / RelevantListing is set.
	RelevantListing *supply.Listing
	// ListingEligibility (Phase 4A.5) is the canonical eligibility of
	// RelevantListing when the candidate was matched (set by the caller that
	// applied the policy; matching itself never evaluates lifecycle).
	ListingEligibility *supply.Eligibility
	WithinBudget       *bool
}

// evidence is the product evidence chosen for a vendor: a fresh signal or
// an ACTIVE listing, reduced to the fields scoring needs.
type evidence struct {
	signal    *supply.Signal
	listing   *supply.Listing
	condition *string
	price     *int64
	// availability points fraction: listing/AVAILABLE = 1.0, CAN_SOURCE = 0.6
	availability float64
	reason       string
}

// Match scores every eligible vendor against req and returns up to
// maxCandidates ranked results at or above minScore. Vendors with no
// relevant category are excluded entirely (eligibility failure, not a
// low score — PRD 2 §17), and the result is never padded with weak
// matches to reach a target count (PRD 2 §26).
//
// listingsByVendor holds each vendor's ACTIVE, unexpired SupplyListings
// (Phase 4A): curated supply evidence used alongside fresh signals. Signals
// already represented by a published or rejected listing should be left out
// of supplyByVendor by the caller (the curation decision supersedes the raw
// evidence). nil keeps the signals-only behaviour.
func Match(
	req *request.BuyerRequest,
	vendors []*vendor.Vendor,
	supplyByVendor map[string][]*supply.Signal,
	listingsByVendor map[string][]supply.MatchableListing,
	weights Weights,
	minScore int,
	maxCandidates int,
) []Candidate {
	reqCategory := strings.ToLower(strings.TrimSpace(deref(req.Category)))
	reqProduct := normalizeProduct(deref(req.Product))
	reqLocation := normalizeLocation(deref(req.Location))
	reqCondition := strings.ToLower(strings.TrimSpace(deref(req.Condition)))

	var candidates []Candidate

	for _, v := range vendors {
		if v.Status != vendor.StatusActive {
			continue // eligibility failure, not scored (PRD 2 §17)
		}

		if reqCategory != "" && !hasCategory(v.Categories, reqCategory) {
			continue // no relevant category -> excluded, not padded in
		}

		signals := supplyByVendor[v.ID.String()]

		breakdown := map[string]int{}
		var reasons []string

		// --- Category (PRD 2 §18) ---
		categoryScore := 0
		if reqCategory != "" && hasCategory(v.Categories, reqCategory) {
			categoryScore = weights.CategoryMax
			reasons = append(reasons, "category_match")
		}
		breakdown["category"] = categoryScore

		// --- Product relevance (PRD 2 §19) ---
		// Signals and ACTIVE listings are both evidence; the strongest wins
		// and a listing wins a tie (it is operator-curated).
		productScore, matchType, relevantSignal := scoreProduct(reqProduct, deref(req.Variant), signals, weights.ProductMax)
		listingScore, listingType, relevantListing, listingCanSource := scoreListings(deref(req.Product), deref(req.Variant), listingsByVendor[v.ID.String()], weights.ProductMax)
		var ev *evidence
		switch {
		case relevantListing != nil && listingScore >= productScore:
			productScore, matchType, relevantSignal = listingScore, listingType, nil
			ev = &evidence{listing: relevantListing, condition: relevantListing.Condition, price: relevantListing.Price,
				availability: 1.0, reason: "active_listing"}
			if listingCanSource {
				ev.availability, ev.reason = 0.6, "can_source_listing"
			}
		case relevantSignal != nil:
			ev = &evidence{signal: relevantSignal, condition: relevantSignal.Condition, price: relevantSignal.Price}
			switch relevantSignal.AvailabilityStatus {
			case supply.AvailabilityAvailable:
				ev.availability, ev.reason = 1.0, "available_supply"
			case supply.AvailabilityCanSource:
				ev.availability, ev.reason = 0.6, "can_source_supply"
			}
		}
		breakdown["product"] = productScore
		if matchType != "" {
			if ev != nil && ev.listing != nil {
				reasons = append(reasons, string(matchType)+"_active_listing")
			} else {
				reasons = append(reasons, string(matchType)+"_supply_signal")
			}
		}

		// --- Location (PRD 2 §21) ---
		locationScore := 0
		if reqLocation != "" {
			if LocationMatches(v.PrimaryLocation, reqLocation) {
				locationScore = weights.LocationMax
				reasons = append(reasons, "location_match")
			} else if hasServiceArea(v.ServiceAreas, reqLocation) {
				locationScore = int(float64(weights.LocationMax) * 0.85)
				reasons = append(reasons, "service_area_match")
			}
		}
		breakdown["location"] = locationScore

		// --- Condition (PRD 2 §22) ---
		conditionScore := 0
		if reqCondition != "" && ev != nil && ev.condition != nil &&
			strings.EqualFold(*ev.condition, reqCondition) {
			conditionScore = weights.ConditionMax
			reasons = append(reasons, "condition_match")
		}
		breakdown["condition"] = conditionScore

		// --- Availability (PRD 2 §23) ---
		availabilityScore := 0
		if ev != nil && ev.availability > 0 {
			availabilityScore = int(float64(weights.AvailabilityMax) * ev.availability)
			reasons = append(reasons, ev.reason)
		}
		breakdown["availability"] = availabilityScore

		total := categoryScore + productScore + locationScore + conditionScore + availabilityScore

		if total < minScore {
			continue
		}

		var withinBudget *bool
		if ev != nil && ev.price != nil && (req.BudgetMax != nil || req.BudgetMin != nil) {
			wb := true
			if req.BudgetMax != nil && *ev.price > *req.BudgetMax {
				wb = false
			}
			withinBudget = &wb
		}

		candidates = append(candidates, Candidate{
			Vendor:          v,
			Score:           total,
			MatchType:       matchTypeOrCategoryOnly(matchType, categoryScore),
			Breakdown:       breakdown,
			Reasons:         reasons,
			RelevantSupply:  relevantSignal,
			RelevantListing: evListing(ev),
			WithinBudget:    withinBudget,
		})
	}

	// Deterministic ranking (PRD 2 §27): score desc, then stronger product
	// relevance, then more recent supply activity, then stable vendor ID.
	sort.SliceStable(candidates, func(i, j int) bool {
		if candidates[i].Score != candidates[j].Score {
			return candidates[i].Score > candidates[j].Score
		}
		if candidates[i].Breakdown["product"] != candidates[j].Breakdown["product"] {
			return candidates[i].Breakdown["product"] > candidates[j].Breakdown["product"]
		}
		// 01a audit: among equally good vendors, the one sent fewer
		// requests so far goes first, so leads rotate instead of always
		// reaching the same vendors (the ID only breaks exact ties).
		if candidates[i].Vendor.RequestsReceived != candidates[j].Vendor.RequestsReceived {
			return candidates[i].Vendor.RequestsReceived < candidates[j].Vendor.RequestsReceived
		}
		return candidates[i].Vendor.ID.String() < candidates[j].Vendor.ID.String()
	})

	if len(candidates) > maxCandidates {
		candidates = candidates[:maxCandidates] // never pad; only ever trim (PRD 2 §26)
	}

	return candidates
}

// scoreProduct implements the PRD 2 §19 product-relevance table.
func scoreProduct(reqProduct, reqVariant string, signals []*supply.Signal, maxPoints int) (int, MatchType, *supply.Signal) {
	if reqProduct == "" {
		return 0, "", nil
	}

	var bestSignal *supply.Signal
	var bestScore int
	var bestType MatchType

	for _, s := range signals {
		if s.NormalizedProduct == nil {
			continue
		}
		sp := *s.NormalizedProduct

		exactProduct := sp == reqProduct
		familyMatch := !exactProduct && signalFamily(sp, reqProduct)

		if !exactProduct && !familyMatch {
			continue
		}

		fresh := s.AvailabilityStatus == supply.AvailabilityAvailable
		canSource := s.AvailabilityStatus == supply.AvailabilityCanSource
		if familyMatch && !fresh && !canSource {
			// Independent review: a loose family match on a signal that is
			// not available is no evidence at all ("iphone" sold out says
			// nothing about an iphone 13 today).
			continue
		}
		variantMatch := reqVariant != "" && s.Variant != nil && strings.EqualFold(*s.Variant, reqVariant)

		var score int
		var mt MatchType

		switch {
		case exactProduct && fresh && variantMatch:
			score, mt = pct(maxPoints, 1.0), MatchSupplyConfirmed
		case exactProduct && fresh:
			score, mt = pct(maxPoints, 0.9), MatchSupplyConfirmed
		case exactProduct && canSource:
			score, mt = pct(maxPoints, 0.67), MatchCanSource
		case exactProduct:
			score, mt = pct(maxPoints, 0.67), MatchProductCapable
		case familyMatch:
			score, mt = pct(maxPoints, 0.5), MatchProductCapable
		}

		if score > bestScore {
			bestScore = score
			bestType = mt
			bestSignal = s
		}
	}

	if bestSignal == nil {
		return 0, "", nil
	}
	return bestScore, bestType, bestSignal
}

// signalFamily: one compact product name extends the other at the start or
// the end ("iphone" / "iphone13", "bags" / "ladiesbags"), and the shorter has
// at least 3 letters. Before the independent review any substring counted,
// so "ac" matched "macbook" and "tv" matched "tvstand".
func signalFamily(a, b string) bool {
	if minLen(a, b) < 3 {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a) || strings.HasSuffix(b, a)
}

func minLen(a, b string) int {
	if len(a) < len(b) {
		return len(a)
	}
	return len(b)
}

func evListing(ev *evidence) *supply.Listing {
	if ev == nil {
		return nil
	}
	return ev.listing
}

// scoreListings scores ACTIVE listings with the same PRD 2 §19 table as
// signals. A published listing is curated, available supply, so it scores
// like a fresh AVAILABLE signal. Products are compared in the listing's
// compact search form (lowercase letters and digits only).
//
// The source signal's product counts only when the listing's product is a
// refinement of it ("bag" -> "Imported ladies bags"); a correction to a
// different product ("iphone 13" -> "iphone 14") drops the old name. A
// listing from a CAN_SOURCE signal scores as can-source supply.
func scoreListings(reqProduct, reqVariant string, listings []supply.MatchableListing, maxPoints int) (int, MatchType, *supply.Listing, bool) {
	rp := supply.NormalizeSearch(reqProduct)
	if rp == "" {
		return 0, "", nil, false
	}
	var best *supply.Listing
	var bestScore int
	var bestType MatchType
	bestCanSource := false
	for _, ml := range listings {
		l := ml.Listing
		if l == nil || l.Status != supply.ListingActive {
			continue
		}
		// The listing's (curated) product or its source signal's product.
		var names []string
		lp := ""
		if l.NormalizedProduct != nil {
			lp = *l.NormalizedProduct
		}
		if lp != "" {
			names = append(names, lp)
		}
		if sp := ml.SourceProduct; sp != "" && sp != lp && (lp == "" || strings.Contains(lp, sp) || strings.Contains(sp, lp)) {
			names = append(names, sp)
		}
		exact, family := false, false
		for _, lp := range names {
			if lp == rp {
				exact = true
			} else if minLen(lp, rp) >= 3 && (strings.Contains(lp, rp) || strings.Contains(rp, lp)) {
				family = true
			}
		}
		variantMatch := reqVariant != "" && l.Variant != nil && strings.EqualFold(*l.Variant, reqVariant)
		var score int
		var mt MatchType
		canSource := ml.SourceAvailability == supply.AvailabilityCanSource
		switch {
		case exact && canSource:
			score, mt = pct(maxPoints, 0.67), MatchCanSource
		case exact && variantMatch:
			score, mt = pct(maxPoints, 1.0), MatchSupplyConfirmed
		case exact:
			score, mt = pct(maxPoints, 0.9), MatchSupplyConfirmed
		case family:
			score, mt = pct(maxPoints, 0.5), MatchProductCapable
		default:
			continue
		}
		if score > bestScore {
			best, bestScore, bestType, bestCanSource = l, score, mt, canSource
		}
	}
	return bestScore, bestType, best, bestCanSource
}

func matchTypeOrCategoryOnly(mt MatchType, categoryScore int) MatchType {
	if mt != "" {
		return mt
	}
	if categoryScore > 0 {
		return MatchCategoryOnly
	}
	return MatchCategoryOnly
}

func pct(max int, frac float64) int {
	return int(float64(max) * frac)
}

func hasCategory(categories []string, target string) bool {
	for _, c := range categories {
		if categoriesCompatible(c, target) {
			return true
		}
	}
	return false
}

// categoryFamilies groups Phase 1's broad extraction category
// ("electronics") with Phase 2's finer-grained vendor tags (PRD 2 §7 lists
// "Electronics, Phones, Laptops, Gaming..." as siblings, but Master PRD §7
// treats them as one umbrella category). Without this, a vendor tagged
// only "phones" would fail eligibility against a request categorized as
// "electronics" — which contradicts the PRD 2 §66 worked example, where
// "OJ Phones" (category: Phones) is expected to remain a candidate for an
// iPhone request categorized as "electronics".
var categoryFamilies = map[string]string{
	"electronics": "electronics",
	"phones":      "electronics",
	"laptops":     "electronics",
	"gaming":      "electronics",
	"tv":          "electronics",
	"computers":   "electronics",
	"accessories": "electronics",
}

func categoryFamily(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	if fam, ok := categoryFamilies[c]; ok {
		return fam
	}
	return c
}

func categoriesCompatible(a, b string) bool {
	return categoryFamily(a) == categoryFamily(b)
}

func hasServiceArea(areas []string, target string) bool {
	for _, a := range areas {
		if LocationMatches(a, target) {
			return true
		}
	}
	return false
}

// LocationMatches: the same place, or one names the other more precisely
// ("Unilag" and "Unilag main gate", "Yaba" and "Yaba, Lagos"): every word
// of the shorter is a word of the longer, and at least one of them is not a
// generic place word ("GRA", "Road", "Phase 1" alone never match).
// Before the 01a audit only identical strings matched, so a buyer in
// "Akoka" or at "Unilag main gate" never reached a vendor saved as "Unilag".
func LocationMatches(a, b string) bool {
	na, nb := normalizeLocation(a), normalizeLocation(b)
	if na == "" || nb == "" {
		return false
	}
	if na == nb {
		return true
	}
	ta, tb := locationWords(na), locationWords(nb)
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	if len(ta) > len(tb) {
		ta, tb = tb, ta
	}
	have := map[string]bool{}
	for _, w := range tb {
		have[w] = true
	}
	specific := false
	for _, w := range ta {
		if !have[w] {
			return false
		}
		specific = specific || !genericPlaceWord(w)
	}
	// "GRA", "Road", "Phase 1" alone exist in many cities: they never make
	// a match on their own (independent review).
	return specific
}

var genericPlaceWords = map[string]bool{"gra": true, "road": true, "rd": true, "street": true, "st": true, "close": true,
	"avenue": true, "ave": true, "estate": true, "phase": true, "island": true, "junction": true, "area": true, "layout": true,
	"extension": true, "quarters": true, "gate": true, "main": true, "hall": true, "hostel": true, "market": true, "park": true,
	"new": true, "old": true, "the": true, "of": true, "and": true, "by": true, "near": true, "off": true}

func genericPlaceWord(w string) bool {
	if genericPlaceWords[w] {
		return true
	}
	for _, r := range w {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// locationWords: lower-case words of letters and digits; "benin" alone is
// read as "benin city" (normalizeLocation).
func locationWords(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

// normalizeProduct matches PRD 2 §19's example: "iPhone 13" / "iphone13" /
// "IPHONE 13" should all be recognized as the same product. No embeddings
// — just case/whitespace normalization.
func normalizeProduct(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), ""))
}

// normalizeLocation handles the PRD 2 §21 example ("Benin" / "Benin City")
// without geographic intelligence infrastructure.
func normalizeLocation(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "benin" {
		return "benin city"
	}
	return s
}

// NormalizeProduct is the exported form of normalizeProduct, reused by
// the offer package for ranking (PRD 3 §18: "should build on existing
// Phase 2 matching rules rather than inventing a second incompatible
// scoring system").
func NormalizeProduct(s string) string { return normalizeProduct(s) }

// NormalizeLocation is the exported form of normalizeLocation, reused by
// the offer package for ranking.
func NormalizeLocation(s string) string { return normalizeLocation(s) }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// CategoriesCompatible is the exported category-family rule, reused by
// Discovery (Phase 4B) so relevance uses the same category semantics as
// matching instead of a second, incompatible scheme.
func CategoriesCompatible(a, b string) bool { return categoriesCompatible(a, b) }

// ProductRelevance compares a wanted product with a listing's product names
// using the same compact search form and exact/family rule scoreListings
// uses (Phase 4B reuse; no embeddings).
func ProductRelevance(want string, names ...string) (exact, family bool) {
	w := supply.NormalizeSearch(want)
	if w == "" {
		return false, false
	}
	for _, n := range names {
		n = supply.NormalizeSearch(n)
		if n == "" {
			continue
		}
		if n == w {
			return true, false
		}
		if strings.Contains(n, w) || strings.Contains(w, n) {
			family = true
		}
	}
	return false, family
}
