// Package discovery is Majo's Discovery capability (Phase 4B, Architecture
// Lock V2 §F): given a viewer context and a scope, which eligible supply is
// relevant, in what order, and why.
//
// Discovery is deliberately small and pure:
//   - it never sends anything, never reads consent, never applies frequency
//     caps, delivery windows or dedupe (that is Distribution, §G);
//   - it never decides lifecycle, freshness or geographic expansion itself:
//     every listing goes through the canonical supply.EligibilityPolicy
//     (Phase 4A.5), including the viewer's geographic reach;
//   - ranking is a deterministic weighted sum with explainable reasons. No
//     ML, embeddings, vector search or learned ranking.
//
// The pipeline is explicit (Engine.Run): candidate supply -> eligibility ->
// retrieval -> filtering -> scoring -> sorting -> result.
package discovery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/supply"
)

// ViewerContext is what the caller knows about the viewer. Discovery never
// fetches it: Distribution builds it from consent and history; a future
// web/app client would build it from the logged-in buyer and a query.
type ViewerContext struct {
	// Location is free text resolved by the canonical geographic policy.
	Location string
	// Interests are declared interest categories (consent record).
	Interests []string
	// RecentCategories / RecentProducts come from the viewer's recent requests.
	RecentCategories []string
	RecentProducts   []string
	// BudgetMax is the most recent stated budget, when known.
	BudgetMax *int64
	// NegativeCategories are categories the viewer said "not interested" to.
	NegativeCategories []string
	// RecentlyShown are listings already shown to the viewer recently. They
	// are only DOWN-RANKED here; Distribution hard-excludes duplicates.
	RecentlyShown map[uuid.UUID]bool
}

// Scope narrows what is being discovered. Not a search engine.
type Scope struct {
	Category string `json:"category,omitempty"`
	Query    string `json:"query,omitempty"`
	PriceMin *int64 `json:"price_min,omitempty"`
	PriceMax *int64 `json:"price_max,omitempty"`
	Limit    int    `json:"limit"`
	// Purpose: "batch" (Distribution), "browse" or "listing-detail" (future surfaces).
	Purpose string `json:"purpose,omitempty"`
}

// DefaultLimit is used when Scope.Limit is not positive.
const DefaultLimit = 3

// SupplyItem is one candidate listing plus the facts Discovery needs that
// are not on the listing row. Media is described, never downloaded.
type SupplyItem struct {
	Listing       *supply.Listing
	VendorActive  bool
	SourceProduct string // source signal product (compact), "" if unknown
	Images        int    // stored image MediaAssets referenced by the listing
	HasVideo      bool   // a stored video MediaAsset is referenced
}

// SupplyQuery is an optional storage pre-filter (an optimisation only: the
// canonical policy and the pipeline decide).
type SupplyQuery struct {
	Category string
}

// SupplyReader loads candidate supply (ACTIVE, inside the lifecycle ceiling
// as a pre-filter; the policy remains authoritative).
type SupplyReader interface {
	DiscoverableSupply(ctx context.Context, q SupplyQuery) ([]SupplyItem, error)
}

// Reason is one explainable scoring contribution.
type Reason struct {
	Dimension string `json:"dimension"`
	Points    int    `json:"points"`
	Detail    string `json:"detail"`
}

func (r Reason) String() string {
	sign := "+"
	if r.Points < 0 {
		sign = ""
	}
	return fmt.Sprintf("%s%d %s", sign, r.Points, r.Detail)
}

// Candidate is a ranked, explainable discovery result.
type Candidate struct {
	ListingID       uuid.UUID          `json:"listing_id"`
	PublicReference string             `json:"public_reference"`
	Product         string             `json:"product"`
	Category        string             `json:"category"`
	Price           *int64             `json:"price"`
	Listing         *supply.Listing    `json:"-"`
	Score           int                `json:"score"`
	Reasons         []Reason           `json:"reasons"`
	Eligibility     supply.Eligibility `json:"eligibility"`
	Reach           supply.ViewerReach `json:"reach"`
	// Notes are non-blocking facts the presenter must respect, e.g.
	// "stale_needs_reconfirmation": never present as guaranteed availability.
	Notes    []string `json:"notes"`
	Images   int      `json:"images"`
	HasVideo bool     `json:"has_video"`
}

// NeedsReconfirmation reports whether availability must be reconfirmed with
// the vendor before it is presented as current (canonical freshness).
func (c Candidate) NeedsReconfirmation() bool {
	for _, n := range c.Notes {
		if n == "stale_needs_reconfirmation" {
			return true
		}
	}
	return false
}

// Exclusion explains why a listing did not become a candidate.
type Exclusion struct {
	ListingID       uuid.UUID `json:"listing_id"`
	PublicReference string    `json:"public_reference"`
	Stage           string    `json:"stage"` // eligibility | retrieval | filter | limit
	Code            string    `json:"code"`
	Detail          string    `json:"detail,omitempty"`
}

// Exclusion stages.
const (
	StageEligibility = "eligibility"
	StageRetrieval   = "retrieval"
	StageFilter      = "filter"
	StageLimit       = "limit"
)

// Exclusion codes added by Discovery (eligibility codes are the canonical
// policy's own reason codes, e.g. lifecycle_expired, vendor_inactive).
const (
	CodeGeoOutOfScope = "geo_out_of_scope"
	CodeGeoUnknown    = "viewer_location_unknown"
	CodeGeoUnresolved = "geo_unresolved"
	CodeCategory      = "category_mismatch"
	CodeQuery         = "query_mismatch"
	CodePriceRange    = "price_out_of_range"
	CodeNegative      = "negative_interest"
	CodeStaleExcluded = "stale_excluded"
	CodeBelowLimit    = "below_limit"
)

// Result is one Discovery run.
type Result struct {
	Candidates     []Candidate `json:"candidates"`
	Exclusions     []Exclusion `json:"exclusions"`
	Considered     int         `json:"considered"`
	RankingVersion string      `json:"ranking_version"`
	EvaluatedAt    time.Time   `json:"evaluated_at"`
}

// Weights are the [PARAM] scoring weights (Lock §F). Negative weights are
// penalties. Every point awarded becomes a Reason.
type Weights struct {
	Category       int `json:"category"`
	Product        int `json:"product"`
	LocationLocal  int `json:"location_local"`
	LocationNearby int `json:"location_nearby"`
	LocationState  int `json:"location_state"`
	LocationRegion int `json:"location_region"`
	Freshness      int `json:"freshness"`
	Media          int `json:"media"`
	Video          int `json:"video"`
	Budget         int `json:"budget"`
	OverBudget     int `json:"over_budget"`
	Quality        int `json:"quality"`
	Exposure       int `json:"exposure"`
}

// DefaultWeights is the initial deterministic ranking (pilot hypothesis).
func DefaultWeights() Weights {
	return Weights{
		Category: 30, Product: 25,
		LocationLocal: 20, LocationNearby: 14, LocationState: 8, LocationRegion: 4,
		Freshness: 10, Media: 10, Video: 5, Budget: 5, OverBudget: -5, Quality: 5, Exposure: -15,
	}
}

// weightKeys maps config keys to fields (DISCOVERY_RANKING_WEIGHTS).
func (w *Weights) field(key string) *int {
	switch key {
	case "category":
		return &w.Category
	case "product":
		return &w.Product
	case "location_local":
		return &w.LocationLocal
	case "location_nearby":
		return &w.LocationNearby
	case "location_state":
		return &w.LocationState
	case "location_region":
		return &w.LocationRegion
	case "freshness":
		return &w.Freshness
	case "media":
		return &w.Media
	case "video":
		return &w.Video
	case "budget":
		return &w.Budget
	case "over_budget":
		return &w.OverBudget
	case "quality":
		return &w.Quality
	case "exposure":
		return &w.Exposure
	}
	return nil
}

// ParseWeights overrides base with "key=value,key=value" (empty = base).
// Unknown keys and non-integers are errors (fail clearly at startup).
func ParseWeights(s string, base Weights) (Weights, error) {
	w := base
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := strings.Cut(part, "=")
		if !ok {
			return base, fmt.Errorf("discovery ranking weights: %q is not key=value", part)
		}
		f := w.field(strings.TrimSpace(k))
		if f == nil {
			return base, fmt.Errorf("discovery ranking weights: unknown dimension %q", strings.TrimSpace(k))
		}
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil || n < -100 || n > 100 {
			return base, fmt.Errorf("discovery ranking weights: %s must be an integer between -100 and 100", strings.TrimSpace(k))
		}
		*f = n
	}
	return w, nil
}

// RankingParams are the deterministic ranking parameters.
type RankingParams struct {
	Weights Weights `json:"weights"`
	// ExcludeStale drops stale listings instead of presenting them with a
	// reconfirmation note. Default false: the canonical policy says stale
	// supply is eligible but needs reconfirmation (Phase 4A.5).
	ExcludeStale bool `json:"exclude_stale"`
}

// DefaultParams are the initial parameters.
func DefaultParams() RankingParams { return RankingParams{Weights: DefaultWeights()} }

// Version identifies the ranking + eligibility configuration that produced a
// result, so a DiscoveryBatch can record what ranked it (not a config system).
func (p RankingParams) Version(policy supply.PolicyConfig) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%+v|%v|%+v", p.Weights, p.ExcludeStale, policy)))
	return "discovery-det-v1+" + hex.EncodeToString(h[:4])
}
