// Package matching implements deterministic, explainable vendor scoring
// against a confirmed BuyerRequest. Per PRD 2 §16 and Middleman_Technical_
// Spec_v1 §4: rule-based only, no ML/embeddings, weights configurable.
package matching

import (
	"sort"
	"strings"

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
	MatchCanSource        MatchType = "CAN_SOURCE"
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
	WithinBudget   *bool
}

// Match scores every eligible vendor against req and returns up to
// maxCandidates ranked results at or above minScore. Vendors with no
// relevant category are excluded entirely (eligibility failure, not a
// low score — PRD 2 §17), and the result is never padded with weak
// matches to reach a target count (PRD 2 §26).
func Match(
	req *request.BuyerRequest,
	vendors []*vendor.Vendor,
	supplyByVendor map[string][]*supply.Signal,
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
		productScore, matchType, relevantSignal := scoreProduct(reqProduct, deref(req.Variant), signals, weights.ProductMax)
		breakdown["product"] = productScore
		if matchType != "" {
			reasons = append(reasons, string(matchType)+"_supply_signal")
		}

		// --- Location (PRD 2 §21) ---
		locationScore := 0
		if reqLocation != "" {
			if normalizeLocation(v.PrimaryLocation) == reqLocation {
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
		if reqCondition != "" && relevantSignal != nil && relevantSignal.Condition != nil &&
			strings.EqualFold(*relevantSignal.Condition, reqCondition) {
			conditionScore = weights.ConditionMax
			reasons = append(reasons, "condition_match")
		}
		breakdown["condition"] = conditionScore

		// --- Availability (PRD 2 §23) ---
		availabilityScore := 0
		if relevantSignal != nil {
			switch relevantSignal.AvailabilityStatus {
			case supply.AvailabilityAvailable:
				availabilityScore = weights.AvailabilityMax
				reasons = append(reasons, "available_supply")
			case supply.AvailabilityCanSource:
				availabilityScore = int(float64(weights.AvailabilityMax) * 0.6)
				reasons = append(reasons, "can_source_supply")
			}
		}
		breakdown["availability"] = availabilityScore

		total := categoryScore + productScore + locationScore + conditionScore + availabilityScore

		if total < minScore {
			continue
		}

		var withinBudget *bool
		if relevantSignal != nil && relevantSignal.Price != nil && (req.BudgetMax != nil || req.BudgetMin != nil) {
			wb := true
			if req.BudgetMax != nil && *relevantSignal.Price > *req.BudgetMax {
				wb = false
			}
			withinBudget = &wb
		}

		candidates = append(candidates, Candidate{
			Vendor:         v,
			Score:          total,
			MatchType:      matchTypeOrCategoryOnly(matchType, categoryScore),
			Breakdown:      breakdown,
			Reasons:        reasons,
			RelevantSupply: relevantSignal,
			WithinBudget:   withinBudget,
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
		familyMatch := !exactProduct && (strings.Contains(sp, reqProduct) || strings.Contains(reqProduct, sp))

		if !exactProduct && !familyMatch {
			continue
		}

		fresh := s.AvailabilityStatus == supply.AvailabilityAvailable
		canSource := s.AvailabilityStatus == supply.AvailabilityCanSource
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
		if normalizeLocation(a) == target {
			return true
		}
	}
	return false
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
