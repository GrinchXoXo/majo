package distribution

import (
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/discovery"
	"github.com/middleman/backend/internal/presentation"
	"github.com/middleman/backend/internal/supply"
)

// BuyerPlan is one audience member's line in a dry run.
type BuyerPlan struct {
	BuyerID        uuid.UUID          `json:"buyer_id"`
	Recipient      string             `json:"recipient"`
	Planned        bool               `json:"planned"`
	Exclusion      string             `json:"exclusion,omitempty"`
	Arm            string             `json:"arm,omitempty"`
	ViewerLocation string             `json:"viewer_location,omitempty"`
	Candidates     []PlannedCandidate `json:"candidates"`
	// ListingExclusions counts why listings were not shown to this buyer.
	ListingExclusions map[string]int `json:"listing_exclusions"`
	Deduped           []string       `json:"deduped,omitempty"` // refs dropped as already shown
	// Excluded lists (ref, code, detail) for diagnostics.
	Excluded []discovery.Exclusion `json:"excluded"`
}

// PlanReport is the dry-run report (also stored on the batch).
type PlanReport struct {
	Mode                Mode            `json:"mode"`
	SendAllowed         bool            `json:"send_allowed"`
	RankingVersion      string          `json:"ranking_version"`
	PresentationVersion string          `json:"presentation_version"`
	Arms                []string        `json:"arms"`
	Scope               discovery.Scope `json:"scope"`
	EvaluatedAt         time.Time       `json:"evaluated_at"`
	WindowOpen          bool            `json:"delivery_window_open"`
	Window              string          `json:"delivery_window,omitempty"`

	SupplyConsidered int `json:"supply_considered"`
	// EligibleListings: listings the canonical policy says are eligible
	// supply now (before any viewer-specific geography).
	EligibleListings int `json:"eligible_listings"`
	// ListingExclusions: distinct listings excluded, by category
	// (expired, stale, geographic, inactive_vendor, ...).
	ListingExclusions map[string]int `json:"listing_exclusions"`

	AudienceConsidered int            `json:"audience_considered"`
	AudienceEligible   int            `json:"audience_eligible"` // permitted + consented + under cap
	Deliveries         int            `json:"deliveries"`
	ExcludedBuyers     map[string]int `json:"excluded_buyers"`
	UnknownRecipients  []string       `json:"unknown_recipients"`
	Buyers             []BuyerPlan    `json:"buyers"`
}

// ExclusionCategory groups exclusion codes for reporting.
func ExclusionCategory(code string) string {
	switch {
	case code == "lifecycle_expired" || code == "status_EXPIRED":
		return "expired"
	case code == discovery.CodeStaleExcluded:
		return "stale"
	case code == "vendor_inactive":
		return "inactive_vendor"
	case code == discovery.CodeGeoOutOfScope || code == discovery.CodeGeoUnknown || code == discovery.CodeGeoUnresolved || code == "geo_scope_not_started":
		return "geographic"
	case code == "not_published" || code == "rejected" || strings.HasPrefix(code, "status_"):
		return "not_live"
	case code == "no_price" || code == "no_product":
		return "incomplete"
	case code == discovery.CodeCategory || code == discovery.CodeQuery || code == discovery.CodePriceRange:
		return "out_of_scope"
	case code == discovery.CodeNegative:
		return "negative_interest"
	case code == ReasonDedupe:
		return "dedupe"
	case code == discovery.CodeBelowLimit:
		return "below_limit"
	}
	return code
}

// ViewerFrom builds the Discovery viewer context from what Distribution
// knows (consent record + history). Discovery never reads consent itself.
func ViewerFrom(m AudienceMember) discovery.ViewerContext {
	v := discovery.ViewerContext{RecentlyShown: map[uuid.UUID]bool{}, NegativeCategories: m.NegativeCategories}
	if m.Preference != nil {
		if m.Preference.Location != nil {
			v.Location = *m.Preference.Location
		}
		v.Interests = append(v.Interests, m.Preference.Interests...)
	}
	for _, r := range m.RecentRequests {
		if v.Location == "" && r.Location != nil {
			v.Location = *r.Location
		}
		if r.Category != nil && *r.Category != "" {
			v.RecentCategories = append(v.RecentCategories, *r.Category)
		}
		if r.Product != nil && *r.Product != "" {
			v.RecentProducts = append(v.RecentProducts, *r.Product)
		}
		if v.BudgetMax == nil && r.BudgetMax != nil {
			b := *r.BudgetMax
			v.BudgetMax = &b
		}
	}
	for id := range m.Exposed {
		v.RecentlyShown[id] = true
	}
	return v
}

// Snapshot records the canonical policy result for a candidate.
func Snapshot(c discovery.Candidate) EligibilitySnapshot {
	e := c.Eligibility
	s := EligibilitySnapshot{
		Eligible: e.Eligible, LifecycleDay: e.Lifecycle.Day, LifecycleMax: e.Lifecycle.MaxDays,
		ExpiresAt: e.Lifecycle.ExpiresAt.UTC().Format(time.RFC3339), Freshness: string(e.Freshness.State),
		GeoStage: string(e.Geography.Stage), GeoReason: e.Geography.Reason,
		ViewerLevel: string(c.Reach.Level), ViewerEligible: c.Reach.Reachable, ViewerReason: c.Reach.Reason,
		Notes: append([]string{}, c.Notes...),
	}
	if e.Freshness.VerifiedAt != nil {
		v := e.Freshness.VerifiedAt.UTC().Format(time.RFC3339)
		s.VerifiedAt = &v
	}
	return s
}

func plannedFrom(c discovery.Candidate) PlannedCandidate {
	l := c.Listing
	pc := PlannedCandidate{
		ListingID: c.ListingID, PublicReference: c.PublicReference, Price: l.Price, Score: c.Score, Reasons: c.Reasons,
		Eligibility: Snapshot(c), Images: c.Images, HasVideo: c.HasVideo, NeedsReconfirmation: c.NeedsReconfirmation(),
	}
	if l.Product != nil {
		pc.Product = *l.Product
	}
	if l.Category != nil {
		pc.Category = *l.Category
	}
	return pc
}

// planner holds what one planning pass needs.
type planner struct {
	cfg    Config
	engine *discovery.Engine
	now    time.Time
}

// Plan computes the audience, calls Discovery per buyer, applies
// consent / gate / caps / dedupe and returns the dry-run report plus the
// deliveries a batch would contain. Pure: no I/O, no sends.
func (p planner) Plan(batchID uuid.UUID, scope discovery.Scope, members []AudienceMember, unknown []string, items []discovery.SupplyItem) (PlanReport, []*Delivery) {
	cfg := p.cfg
	arms := make([]string, len(cfg.Arms))
	for i, a := range cfg.Arms {
		arms[i] = string(a)
	}
	r := PlanReport{
		Mode: cfg.Mode, SendAllowed: cfg.CanSend(), RankingVersion: cfg.Ranking.Version(p.engine.Policy().Config()),
		PresentationVersion: presentation.Version, Arms: arms, Scope: scope, EvaluatedAt: p.now,
		WindowOpen: cfg.Window.Allows(p.now), Window: cfg.Window.Raw,
		SupplyConsidered: len(items), ListingExclusions: map[string]int{}, ExcludedBuyers: map[string]int{},
		UnknownRecipients: append([]string{}, unknown...), Buyers: []BuyerPlan{}, AudienceConsidered: len(members) + len(unknown),
	}
	for range unknown {
		r.ExcludedBuyers[ReasonUnknownBuyer]++
	}

	// Viewer-independent eligibility (canonical policy) for the supply.
	excludedListing := map[string]map[uuid.UUID]bool{}
	markListing := func(code string, id uuid.UUID) {
		cat := ExclusionCategory(code)
		if cat == "below_limit" {
			return
		}
		if excludedListing[cat] == nil {
			excludedListing[cat] = map[uuid.UUID]bool{}
		}
		excludedListing[cat][id] = true
	}
	for _, it := range items {
		active := it.VendorActive
		if p.engine.Policy().EvaluateListing(it.Listing, &active, p.now).Eligible {
			r.EligibleListings++
		}
	}

	limit := scope.Limit
	if limit <= 0 || limit > cfg.ListingsPerDelivery {
		limit = cfg.ListingsPerDelivery
	}
	var deliveries []*Delivery
	for _, m := range members {
		bp := BuyerPlan{BuyerID: m.BuyerID, Recipient: m.Recipient, ListingExclusions: map[string]int{},
			Candidates: []PlannedCandidate{}, Excluded: []discovery.Exclusion{}}
		exclude := func(reason string) {
			bp.Exclusion = reason
			r.ExcludedBuyers[reason]++
			r.Buyers = append(r.Buyers, bp)
		}
		if ok, reason := cfg.Permits(m.Recipient); !ok {
			exclude(reason)
			continue
		}
		switch {
		case m.Preference == nil:
			exclude(ReasonNoConsent)
			continue
		case m.Preference.Status != OptedIn:
			exclude(ReasonOptedOut)
			continue
		case m.DeliveriesInCapPeriod >= cfg.MaxDeliveriesPerBuyer:
			exclude(ReasonFrequencyCap)
			continue
		}
		r.AudienceEligible++

		viewer := ViewerFrom(m)
		bp.ViewerLocation = viewer.Location
		run := scope
		run.Limit = len(items) + 1 // rank everything; dedupe and the per-delivery limit apply below
		res := p.engine.Run(items, viewer, run, cfg.Ranking, p.now)
		for _, ex := range res.Exclusions {
			cat := ExclusionCategory(ex.Code)
			bp.ListingExclusions[cat]++
			bp.Excluded = append(bp.Excluded, ex)
			markListing(ex.Code, ex.ListingID)
		}
		var picked []discovery.Candidate
		for _, c := range res.Candidates {
			if _, shown := m.Exposed[c.ListingID]; shown {
				bp.Deduped = append(bp.Deduped, c.PublicReference)
				bp.ListingExclusions["dedupe"]++
				markListing(ReasonDedupe, c.ListingID)
				continue
			}
			if len(picked) < limit {
				picked = append(picked, c)
			}
		}
		if len(picked) == 0 {
			if len(bp.Deduped) > 0 && len(res.Candidates) == len(bp.Deduped) {
				exclude(ReasonDedupe)
			} else {
				exclude(ReasonNoCandidates)
			}
			continue
		}
		arm := presentation.AssignArm(batchID, m.BuyerID, cfg.Arms)
		d := &Delivery{
			ID: uuid.New(), BatchID: batchID, BuyerID: m.BuyerID, Recipient: m.Recipient, Arm: string(arm),
			Status: DeliveryPlanned, DedupeKey: DeliveryDedupeKey(batchID, m.BuyerID), ConsentStatus: string(m.Preference.Status),
			PlannedAt: p.now,
		}
		pid := m.Preference.ID
		d.PreferenceID = &pid
		for _, c := range picked {
			pc := plannedFrom(c)
			d.Candidates = append(d.Candidates, pc)
			d.ListingIDs = append(d.ListingIDs, c.ListingID)
		}
		bp.Planned, bp.Arm, bp.Candidates = true, string(arm), d.Candidates
		r.Buyers = append(r.Buyers, bp)
		deliveries = append(deliveries, d)
	}
	for cat, ids := range excludedListing {
		r.ListingExclusions[cat] = len(ids)
	}
	r.Deliveries = len(deliveries)
	sort.SliceStable(r.Buyers, func(i, j int) bool { return r.Buyers[i].Recipient < r.Buyers[j].Recipient })
	return r, deliveries
}

// cardsFor renders buyer-safe cards from fresh candidates (presentation
// never sees vendor identity or internal IDs other than the asset IDs it
// needs for media).
func cardsFor(cands []discovery.Candidate, media map[uuid.UUID]listingMedia) []presentation.Card {
	cards := make([]presentation.Card, 0, len(cands))
	for _, c := range cands {
		l := c.Listing
		card := presentation.Card{Ref: l.PublicReference, Price: l.Price, NeedsReconfirmation: c.NeedsReconfirmation()}
		if l.Product != nil {
			card.Product = *l.Product
		}
		if l.Category != nil {
			card.Category = *l.Category
		}
		if l.Condition != nil {
			card.Condition = *l.Condition
		}
		if l.Location != nil {
			card.Area = *l.Location
		}
		if m, ok := media[l.ID]; ok {
			if c.Images > 0 {
				card.Image = m.image
			}
			if c.HasVideo {
				card.Video = m.video
			}
		}
		cards = append(cards, card)
	}
	return cards
}

// listingMedia are the stored assets a card may present.
type listingMedia struct {
	image *uuid.UUID
	video *uuid.UUID
}

// mediaOf picks the primary image and first video from a listing's references.
func mediaOf(l *supply.Listing) listingMedia {
	var out listingMedia
	if p := supply.PrimaryMedia(l.MediaReferences); p != nil && p.AssetID != "" {
		if id, err := uuid.Parse(p.AssetID); err == nil {
			out.image = &id
		}
	}
	for _, r := range l.MediaReferences {
		if r.Type == "video" && r.AssetID != "" {
			if id, err := uuid.Parse(r.AssetID); err == nil {
				out.video = &id
				break
			}
		}
	}
	return out
}
