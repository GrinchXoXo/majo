package discovery

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/middleman/backend/internal/geo"
	"github.com/middleman/backend/internal/matching"
	"github.com/middleman/backend/internal/supply"
)

// Engine runs the Discovery pipeline. It holds only the canonical
// eligibility policy; it has no storage, clock or channel dependencies.
type Engine struct {
	policy *supply.EligibilityPolicy
}

// NewEngine builds an engine over the canonical policy (nil = default).
func NewEngine(policy *supply.EligibilityPolicy) *Engine {
	if policy == nil {
		policy = supply.MustDefaultPolicy()
	}
	return &Engine{policy: policy}
}

// Policy returns the canonical policy the engine consumes.
func (e *Engine) Policy() *supply.EligibilityPolicy { return e.policy }

// working is a listing moving through the pipeline.
type working struct {
	item  SupplyItem
	elig  supply.Eligibility
	reach supply.ViewerReach
	cand  Candidate
}

// Run executes the pipeline. Pure and deterministic: the same items,
// viewer, scope, params and now always give the same result.
func (e *Engine) Run(items []SupplyItem, viewer ViewerContext, scope Scope, params RankingParams, now time.Time) Result {
	r := Result{Considered: len(items), RankingVersion: params.Version(e.policy.Config()), EvaluatedAt: now,
		Candidates: []Candidate{}, Exclusions: []Exclusion{}}
	pool := e.eligibilityStage(items, viewer, now, &r)
	pool = retrievalStage(pool, scope, &r)
	pool = filterStage(pool, viewer, params, &r)
	scoreStage(pool, viewer, scope, params.Weights)
	sortStage(pool)
	limitStage(pool, scope, &r)
	return r
}

func exclude(r *Result, w working, stage, code, detail string) {
	r.Exclusions = append(r.Exclusions, Exclusion{
		ListingID: w.item.Listing.ID, PublicReference: w.item.Listing.PublicReference, Stage: stage, Code: code, Detail: detail,
	})
}

// 1. Eligibility: the canonical policy decides (lifecycle, status, vendor,
// price, geographic stage) and the same policy decides whether the current
// geographic scope reaches the viewer. Nothing here re-derives either.
func (e *Engine) eligibilityStage(items []SupplyItem, viewer ViewerContext, now time.Time, r *Result) []working {
	out := make([]working, 0, len(items))
	for _, it := range items {
		if it.Listing == nil {
			continue
		}
		active := it.VendorActive
		el, reach := e.policy.EvaluateForViewer(supply.FactsFromListing(it.Listing, &active), viewer.Location, now)
		w := working{item: it, elig: el, reach: reach}
		switch {
		case !el.Eligible:
			exclude(r, w, StageEligibility, el.Reasons[0], strings.Join(el.Reasons, ","))
		case !reach.Known:
			exclude(r, w, StageEligibility, CodeGeoUnknown, reach.Reason)
		case !reach.Reachable && reach.Level == "":
			code := CodeGeoOutOfScope
			if !reach.Viewer.Resolved || !el.Geography.Origin.Resolved {
				code = CodeGeoUnresolved
			}
			exclude(r, w, StageEligibility, code, reach.Reason)
		case !reach.Reachable:
			exclude(r, w, StageEligibility, CodeGeoOutOfScope, reach.Reason)
		default:
			out = append(out, w)
		}
	}
	return out
}

// 2. Retrieval: what the scope asks for (category family, query, price range).
func retrievalStage(pool []working, scope Scope, r *Result) []working {
	out := pool[:0:0]
	for _, w := range pool {
		l := w.item.Listing
		if scope.Category != "" && (l.Category == nil || !matching.CategoriesCompatible(*l.Category, scope.Category)) {
			exclude(r, w, StageRetrieval, CodeCategory, "scope category "+scope.Category)
			continue
		}
		if scope.Query != "" {
			exact, family := matching.ProductRelevance(scope.Query, productNames(w.item)...)
			if !exact && !family {
				exclude(r, w, StageRetrieval, CodeQuery, "query "+scope.Query)
				continue
			}
		}
		if l.Price != nil && ((scope.PriceMin != nil && *l.Price < *scope.PriceMin) || (scope.PriceMax != nil && *l.Price > *scope.PriceMax)) {
			exclude(r, w, StageRetrieval, CodePriceRange, fmt.Sprintf("price %d", *l.Price))
			continue
		}
		out = append(out, w)
	}
	return out
}

// 3. Filtering: viewer negative interests, and the stale rule. Stale
// supply stays in by default (canonical policy: eligible, reconfirm first).
func filterStage(pool []working, viewer ViewerContext, params RankingParams, r *Result) []working {
	out := pool[:0:0]
	for _, w := range pool {
		l := w.item.Listing
		if l.Category != nil && anyCompatible(viewer.NegativeCategories, *l.Category) {
			exclude(r, w, StageFilter, CodeNegative, "viewer not interested in "+*l.Category)
			continue
		}
		if params.ExcludeStale && w.elig.Freshness.State != supply.Fresh {
			exclude(r, w, StageFilter, CodeStaleExcluded, string(w.elig.Freshness.State))
			continue
		}
		out = append(out, w)
	}
	return out
}

// 4. Scoring: deterministic weighted sum; every contribution is a Reason.
func scoreStage(pool []working, viewer ViewerContext, scope Scope, wt Weights) {
	for i := range pool {
		w := &pool[i]
		l := w.item.Listing
		var reasons []Reason
		add := func(dim string, pts int, detail string) {
			if pts != 0 {
				reasons = append(reasons, Reason{Dimension: dim, Points: pts, Detail: detail})
			}
		}

		// Category: the scope category, declared interests, recent requests.
		if l.Category != nil {
			wanted := append(append(append([]string{}, nonEmpty(scope.Category)...), viewer.Interests...), viewer.RecentCategories...)
			for _, c := range wanted {
				if matching.CategoriesCompatible(*l.Category, c) {
					add("category", wt.Category, "category match ("+*l.Category+" ~ "+c+")")
					break
				}
			}
		}
		// Product: the scope query or the viewer's recent request products.
		wantedProducts := append(nonEmpty(scope.Query), viewer.RecentProducts...)
		best := 0
		detail := ""
		for _, p := range wantedProducts {
			exact, family := matching.ProductRelevance(p, productNames(w.item)...)
			switch {
			case exact && wt.Product > best:
				best, detail = wt.Product, "product match ("+p+")"
			case family && wt.Product/2 > best:
				best, detail = wt.Product/2, "related product ("+p+")"
			}
		}
		add("product", best, detail)

		// Location: from the canonical reach level, with the stage's reason.
		switch w.reach.Level {
		case geo.LevelExactArea, geo.LevelCommunity, geo.LevelLGA:
			add("location", wt.LocationLocal, "local supply (buyer in origin area)")
		case geo.LevelCity:
			add("location", wt.LocationNearby, "nearby supply ("+w.elig.Geography.Reason+")")
		case geo.LevelState:
			add("location", wt.LocationState, "statewide supply ("+w.elig.Geography.Reason+")")
		case geo.LevelRegion:
			add("location", wt.LocationRegion, "regional supply ("+w.elig.Geography.Reason+")")
		}

		// Freshness: the canonical freshness result.
		if w.elig.Freshness.State == supply.Fresh {
			add("freshness", wt.Freshness, fmt.Sprintf("fresh verification (%dh ago)", w.elig.Freshness.AgeHours))
		}

		// Media completeness: stored assets only, never downloaded here.
		if w.item.Images > 0 {
			add("media", wt.Media, fmt.Sprintf("media complete (%d image(s))", w.item.Images))
		}
		if w.item.HasVideo {
			add("video", wt.Video, "video available")
		}

		// Budget: preferred when within budget, never excluded for it.
		if viewer.BudgetMax != nil && l.Price != nil {
			if *l.Price <= *viewer.BudgetMax {
				add("budget", wt.Budget, "within budget")
			} else {
				add("budget", wt.OverBudget, fmt.Sprintf("over budget (%d > %d)", *l.Price, *viewer.BudgetMax))
			}
		}

		// Listing quality: complete buyer-facing details.
		if l.Condition != nil && *l.Condition != "" && l.Location != nil && *l.Location != "" && l.Category != nil {
			add("quality", wt.Quality, "complete listing details")
		}

		// Prior exposure: down-rank only (Distribution hard-dedupes).
		if viewer.RecentlyShown[l.ID] {
			add("exposure", wt.Exposure, "shown to this viewer recently")
		}

		score := 0
		for _, rs := range reasons {
			score += rs.Points
		}
		w.cand = Candidate{
			ListingID: l.ID, PublicReference: l.PublicReference, Listing: l, Score: score, Reasons: reasons,
			Eligibility: w.elig, Reach: w.reach, Notes: append([]string{}, w.elig.Notes...),
			Images: w.item.Images, HasVideo: w.item.HasVideo,
		}
		if l.Product != nil {
			w.cand.Product = *l.Product
		}
		if l.Category != nil {
			w.cand.Category = *l.Category
		}
		w.cand.Price = l.Price
		if w.cand.Reasons == nil {
			w.cand.Reasons = []Reason{}
		}
	}
}

// 5. Sorting: score, then most recently verified, then most recently
// published, then the public reference: a total, deterministic order.
func sortStage(pool []working) {
	sort.SliceStable(pool, func(i, j int) bool {
		a, b := pool[i].cand, pool[j].cand
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if ta, tb := timeOr(a.Listing.LastVerifiedAt), timeOr(b.Listing.LastVerifiedAt); !ta.Equal(tb) {
			return ta.After(tb)
		}
		if ta, tb := timeOr(a.Listing.PublishedAt), timeOr(b.Listing.PublishedAt); !ta.Equal(tb) {
			return ta.After(tb)
		}
		return a.PublicReference < b.PublicReference
	})
}

// 6. Result: the top Limit candidates; the rest are recorded as excluded by limit.
func limitStage(pool []working, scope Scope, r *Result) {
	limit := scope.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	for i, w := range pool {
		if i < limit {
			r.Candidates = append(r.Candidates, w.cand)
		} else {
			exclude(r, w, StageLimit, CodeBelowLimit, fmt.Sprintf("score %d", w.cand.Score))
		}
	}
}

func productNames(it SupplyItem) []string {
	var names []string
	if it.Listing.Product != nil {
		names = append(names, *it.Listing.Product)
	}
	if it.SourceProduct != "" {
		names = append(names, it.SourceProduct)
	}
	return names
}

func anyCompatible(cats []string, c string) bool {
	for _, x := range cats {
		if x != "" && matching.CategoriesCompatible(x, c) {
			return true
		}
	}
	return false
}

func nonEmpty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}

func timeOr(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// Service is the application boundary: load candidate supply, then run the
// pure engine. It still sends nothing and reads no consent.
type Service struct {
	engine *Engine
	reader SupplyReader
	now    func() time.Time
}

// NewService builds the Discovery service.
func NewService(engine *Engine, reader SupplyReader) *Service {
	return &Service{engine: engine, reader: reader, now: time.Now}
}

// SetClock replaces the clock (tests).
func (s *Service) SetClock(now func() time.Time) { s.now = now }

// Engine exposes the pure engine (callers ranking many viewers over one
// supply snapshot load once and rank per viewer).
func (s *Service) Engine() *Engine { return s.engine }

// Load returns the candidate supply for a scope (one read per batch).
func (s *Service) Load(ctx context.Context, scope Scope) ([]SupplyItem, error) {
	return s.reader.DiscoverableSupply(ctx, SupplyQuery{})
}

// Now is the instant Discovery evaluates at.
func (s *Service) Now() time.Time { return s.now().UTC() }

// Candidates is Discovery.Candidates(viewer, scope, params) (Lock §F).
func (s *Service) Candidates(ctx context.Context, viewer ViewerContext, scope Scope, params RankingParams) (Result, error) {
	items, err := s.Load(ctx, scope)
	if err != nil {
		return Result{}, fmt.Errorf("loading discoverable supply: %w", err)
	}
	return s.engine.Run(items, viewer, scope, params, s.Now()), nil
}
