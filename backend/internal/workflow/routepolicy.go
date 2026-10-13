package workflow

import (
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/matching"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/supply"
)

// Routing exclusion reasons (recorded on the decision).
const (
	RouteBelowScore        = "below_score"
	RouteVendorBusy        = "vendor_open_request_limit"
	RouteListingIneligible = "listing_not_eligible"
	RouteNotLocal          = "outside_buyer_area"
	RouteFanOutLimit       = "fan_out_limit"
)

// RoutePlan is the deterministic routing decision for one request.
type RoutePlan struct {
	Route    []matching.Candidate // contact now, in order
	NonLocal []matching.Candidate // eligible but outside the buyer's area (ask before contacting)
	Excluded map[string]string    // vendor id -> reason
}

// RoutingPolicy chooses which matched vendors to contact and in what order.
// It never contacts every match: it applies the score floor, the canonical
// listing eligibility, a per-vendor open-request limit and a fan-out limit;
// vendors outside the buyer's area are held back unless the canonical
// geographic expansion policy already makes their listing reachable for the
// buyer (progressive expansion), in which case they count as local.
func RoutingPolicy(req *request.BuyerRequest, cands []matching.Candidate, openByVendor map[uuid.UUID]int,
	policy *supply.EligibilityPolicy, p autonomy.Params, now time.Time) RoutePlan {
	plan := RoutePlan{Excluded: map[string]string{}}
	loc := strings.TrimSpace(deref(req.Location))
	var local, nonLocal []matching.Candidate
	for _, c := range cands {
		id := c.Vendor.ID.String()
		switch {
		case c.Score < p.RouteMinScore:
			plan.Excluded[id] = RouteBelowScore
			continue
		case c.ListingEligibility != nil && !c.ListingEligibility.Eligible:
			plan.Excluded[id] = RouteListingIneligible
			continue
		case openByVendor[c.Vendor.ID] >= p.RouteVendorMaxOpen:
			plan.Excluded[id] = RouteVendorBusy
			continue
		}
		if loc == "" || c.Breakdown["location"] > 0 || listingReaches(c, loc, policy, now) {
			local = append(local, c)
		} else {
			nonLocal = append(nonLocal, c)
		}
	}
	order := func(cs []matching.Candidate) {
		// Matching order (score, product evidence, vendor id) with evidence
		// priced above the buyer's budget moved after the rest.
		sort.SliceStable(cs, func(i, j int) bool {
			oi, oj := overBudget(cs[i]), overBudget(cs[j])
			return !oi && oj
		})
	}
	order(local)
	order(nonLocal)
	for i, c := range local {
		if i < p.RouteMaxVendors {
			plan.Route = append(plan.Route, c)
		} else {
			plan.Excluded[c.Vendor.ID.String()] = RouteFanOutLimit
		}
	}
	for i, c := range nonLocal {
		if i < p.RouteMaxVendors {
			plan.NonLocal = append(plan.NonLocal, c)
		}
		if _, set := plan.Excluded[c.Vendor.ID.String()]; !set {
			plan.Excluded[c.Vendor.ID.String()] = RouteNotLocal
		}
	}
	return plan
}

func overBudget(c matching.Candidate) bool { return c.WithinBudget != nil && !*c.WithinBudget }

// listingReaches: the candidate's evidence is a listing the canonical policy
// says this buyer may already see at its current expansion stage.
func listingReaches(c matching.Candidate, viewerLocation string, policy *supply.EligibilityPolicy, now time.Time) bool {
	if c.RelevantListing == nil || policy == nil {
		return false
	}
	active := true
	e, reach := policy.EvaluateForViewer(supply.FactsFromListing(c.RelevantListing, &active), viewerLocation, now)
	return e.Eligible && reach.Reachable
}

// Explain summarises a candidate for the decision log.
func Explain(c matching.Candidate) map[string]any {
	m := map[string]any{"vendor_id": c.Vendor.ID.String(), "vendor": c.Vendor.BusinessName, "score": c.Score,
		"match_type": string(c.MatchType), "breakdown": c.Breakdown, "reasons": c.Reasons}
	if c.RelevantListing != nil {
		m["listing_reference"] = c.RelevantListing.PublicReference
	}
	if c.WithinBudget != nil {
		m["within_budget"] = *c.WithinBudget
	}
	return m
}
