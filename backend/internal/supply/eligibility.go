package supply

// Phase 4A.5: supply lifecycle, freshness and progressive geographic
// expansion: the ONE canonical supply-eligibility policy.
//
// Four separate concepts, deliberately not conflated:
//
//   - Media retrieval window (provider constraint, ~7 days for WhatsApp):
//     how long Majo can DOWNLOAD inbound media. Handled by the media
//     ingestion job (4A.1). Irrelevant once the MediaAsset is stored, and
//     never used here.
//   - Freshness: how recently the supply was VERIFIED (last_verified_at).
//     Derived, never persisted as a state. Stale does not mean expired:
//     a stale listing stays eligible, flagged for reconfirmation.
//   - Lifecycle: how long a published listing stays usable marketplace
//     inventory. Starts at publication; hard ceiling [PARAM] (default 60
//     days), persisted once in supply_listings.expires_at when the
//     lifecycle starts and never extended by later evidence.
//   - Geographic expansion: how widely the listing may be searched against
//     demand at its current lifecycle age (LOCAL -> NEARBY -> STATE ->
//     REGION -> EXPIRED). Derived from timestamps + policy, never stored.
//
// Matching uses Evaluate today; Discovery (4B) must use the same Evaluate
// and must not define its own lifecycle rules. Evaluate is pure: no
// database, no network, no clock (now is an argument), no randomness.
// Future intelligence (category, price, transportability, demand) plugs in
// by implementing LifecyclePolicy / FreshnessPolicy /
// GeographicExpansionPolicy; ListingFacts already carries the attributes
// such a policy needs.

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/geo"
)

const day = 24 * time.Hour

// PolicyConfig holds the [PARAM] values of the deterministic policy.
type PolicyConfig struct {
	// MaxLifecycle is the hard ceiling of a listing's marketplace lifecycle,
	// counted from publication. Default 60 days.
	MaxLifecycle time.Duration
	// FreshnessWindow: verification older than this makes a listing STALE.
	// Default 72h (Lock V2 §D pilot hypothesis).
	FreshnessWindow time.Duration
	// Lifecycle ages (since publication) at which each geographic stage
	// begins. Defaults 0 / 20 / 30 / 45 days.
	GeoLocalAfter  time.Duration
	GeoNearbyAfter time.Duration
	GeoStateAfter  time.Duration
	GeoRegionAfter time.Duration
}

// DefaultPolicyConfig is the initial Majo policy (all values [PARAM]).
func DefaultPolicyConfig() PolicyConfig {
	return PolicyConfig{
		MaxLifecycle: 60 * day, FreshnessWindow: 72 * time.Hour,
		GeoLocalAfter: 0, GeoNearbyAfter: 20 * day, GeoStateAfter: 30 * day, GeoRegionAfter: 45 * day,
	}
}

// Validate rejects impossible policies (stages out of order, a stage that
// would begin after the listing expired, non-positive windows).
func (c PolicyConfig) Validate() error {
	d := fmtDays
	switch {
	case c.MaxLifecycle <= 0:
		return fmt.Errorf("supply lifecycle: max lifecycle must be > 0 (got %s)", d(c.MaxLifecycle))
	case c.FreshnessWindow <= 0:
		return fmt.Errorf("supply lifecycle: freshness window must be > 0 (got %s)", c.FreshnessWindow)
	case c.GeoLocalAfter < 0:
		return fmt.Errorf("supply lifecycle: local stage start must be >= 0 (got %s)", d(c.GeoLocalAfter))
	case c.GeoNearbyAfter < c.GeoLocalAfter:
		return fmt.Errorf("supply lifecycle: nearby stage (%s) must not start before local (%s)", d(c.GeoNearbyAfter), d(c.GeoLocalAfter))
	case c.GeoStateAfter < c.GeoNearbyAfter:
		return fmt.Errorf("supply lifecycle: state stage (%s) must not start before nearby (%s)", d(c.GeoStateAfter), d(c.GeoNearbyAfter))
	case c.GeoRegionAfter < c.GeoStateAfter:
		return fmt.Errorf("supply lifecycle: region stage (%s) must not start before state (%s)", d(c.GeoRegionAfter), d(c.GeoStateAfter))
	case c.MaxLifecycle < c.GeoRegionAfter:
		return fmt.Errorf("supply lifecycle: max lifecycle (%s) must not end before the region stage starts (%s)", d(c.MaxLifecycle), d(c.GeoRegionAfter))
	}
	return nil
}

// fmtDays renders whole-day durations as "20d" (config is in days).
func fmtDays(x time.Duration) string {
	if x%day == 0 {
		return fmt.Sprintf("%dd", int64(x/day))
	}
	return x.String()
}

// ---- facts ----------------------------------------------------------------

// ListingFacts is everything a policy may look at. It is a plain value so
// policies stay pure; category/price/condition are here for future
// category- or value-aware policies even though the initial policy ignores
// them.
type ListingFacts struct {
	ListingID      uuid.UUID
	Status         ListingStatus
	Rejected       bool
	CreatedAt      time.Time
	PublishedAt    *time.Time
	LastVerifiedAt *time.Time
	ExpiresAt      time.Time // persisted lifecycle ceiling
	Product        *string
	Category       *string
	Condition      *string
	Price          *int64
	Location       *string // origin (listing location; already falls back to the vendor's)
	VendorID       uuid.UUID
	// VendorActive is nil when the caller does not know the vendor's status.
	VendorActive *bool
}

// FactsFromListing builds the policy input from a listing.
func FactsFromListing(l *Listing, vendorActive *bool) ListingFacts {
	return ListingFacts{
		ListingID: l.ID, Status: l.Status, Rejected: l.RejectedAt != nil, CreatedAt: l.CreatedAt,
		PublishedAt: l.PublishedAt, LastVerifiedAt: l.LastVerifiedAt, ExpiresAt: l.ExpiresAt,
		Product: l.Product, Category: l.Category, Condition: l.Condition, Price: l.Price, Location: l.Location,
		VendorID: l.VendorID, VendorActive: vendorActive,
	}
}

// ---- results --------------------------------------------------------------

type LifecycleState string

const (
	LifecycleNotPublished LifecycleState = "NOT_PUBLISHED" // DRAFT: the lifecycle has not started
	LifecycleLive         LifecycleState = "LIVE"          // published, inside the ceiling
	LifecycleExpired      LifecycleState = "EXPIRED"       // past the ceiling (even if status still says ACTIVE)
	LifecycleClosed       LifecycleState = "CLOSED"        // SOLD / UNAVAILABLE / ARCHIVED / EXPIRED status
)

type FreshnessState string

const (
	Fresh      FreshnessState = "FRESH"
	Stale      FreshnessState = "STALE"
	Unverified FreshnessState = "UNVERIFIED"
)

type GeoStage string

const (
	GeoNone    GeoStage = "NONE"    // not searchable (not published or closed)
	GeoPending GeoStage = "PENDING" // published, local stage not reached yet (local start > 0)
	GeoLocal   GeoStage = "LOCAL"
	GeoNearby  GeoStage = "NEARBY"
	GeoState   GeoStage = "STATE"
	GeoRegion  GeoStage = "REGION"
	GeoExpired GeoStage = "EXPIRED"
)

type LifecycleView struct {
	State     LifecycleState `json:"state"`
	StartedAt *time.Time     `json:"started_at"` // publication; nil while DRAFT
	// Age is since StartedAt (since creation for a draft).
	Age       time.Duration `json:"-"`
	Day       int           `json:"day"`      // whole days of Age
	MaxDays   int           `json:"max_days"` // this listing's ceiling in days
	ExpiresAt time.Time     `json:"expires_at"`
	Expired   bool          `json:"expired"`
}

type FreshnessView struct {
	State      FreshnessState `json:"state"`
	VerifiedAt *time.Time     `json:"verified_at"` // the freshness reference time (last_verified_at)
	Age        time.Duration  `json:"-"`
	AgeHours   int            `json:"age_hours"`
	Window     time.Duration  `json:"-"`
	StaleAt    *time.Time     `json:"stale_at"`
}

type GeoScope struct {
	Stage  GeoStage    `json:"stage"`
	Scope  string      `json:"scope"`  // ORIGIN_AREA | NEARBY_AREAS | STATE | REGION | NONE
	Levels []geo.Level `json:"levels"` // geographic levels eligible now
	Origin geo.Place   `json:"origin"`
	Reason string      `json:"reason"`
	// NextStage / NextExpansionAt: the next change (EXPIRED at the ceiling).
	NextStage       GeoStage   `json:"next_stage,omitempty"`
	NextExpansionAt *time.Time `json:"next_expansion_at,omitempty"`
}

// Eligibility is the canonical answer for one listing at one instant.
type Eligibility struct {
	// Eligible: may be used as marketplace supply now (matching, and
	// discovery in 4B). Stale listings ARE eligible (flagged in Notes).
	Eligible bool `json:"eligible"`
	// Reasons are the blocking reasons when not eligible (stable codes).
	Reasons []string `json:"reasons"`
	// Notes are non-blocking facts consumers should act on
	// (e.g. stale_needs_reconfirmation, origin_unresolved).
	Notes     []string      `json:"notes"`
	Status    ListingStatus `json:"status"`
	Lifecycle LifecycleView `json:"lifecycle"`
	Freshness FreshnessView `json:"freshness"`
	Geography GeoScope      `json:"geography"`
	// EvaluatedAt is the instant the result describes.
	EvaluatedAt time.Time `json:"evaluated_at"`
}

// ---- policy interfaces ------------------------------------------------------

// LifecyclePolicy decides the lifecycle and the ceiling a new lifecycle gets.
type LifecyclePolicy interface {
	Lifecycle(f ListingFacts, now time.Time) LifecycleView
	// CeilingFrom is the expires_at to persist when a lifecycle starts at start.
	CeilingFrom(start time.Time) time.Time
}

// FreshnessPolicy decides how fresh the verification evidence is.
type FreshnessPolicy interface {
	Freshness(f ListingFacts, now time.Time) FreshnessView
}

// GeographicExpansionPolicy decides the geographic scope at a lifecycle point.
type GeographicExpansionPolicy interface {
	Scope(f ListingFacts, lc LifecycleView, now time.Time) GeoScope
}

// EligibilityPolicy composes the three. It is the one entry point.
type EligibilityPolicy struct {
	lifecycle LifecyclePolicy
	freshness FreshnessPolicy
	geography GeographicExpansionPolicy
	cfg       PolicyConfig
}

// NewEligibilityPolicy builds the deterministic policy from cfg (validated)
// and a location resolver (nil = the Nigeria pilot gazetteer).
func NewEligibilityPolicy(cfg PolicyConfig, resolver geo.Resolver) (*EligibilityPolicy, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if resolver == nil {
		resolver = geo.NewStaticResolver(geo.NigeriaPilot)
	}
	return &EligibilityPolicy{
		lifecycle: CeilingLifecycle{Max: cfg.MaxLifecycle},
		freshness: WindowFreshness{Window: cfg.FreshnessWindow},
		geography: NewStagedExpansion(cfg, resolver),
		cfg:       cfg,
	}, nil
}

// MustDefaultPolicy is the default policy (for tests and fallbacks).
func MustDefaultPolicy() *EligibilityPolicy {
	p, err := NewEligibilityPolicy(DefaultPolicyConfig(), nil)
	if err != nil {
		panic(err)
	}
	return p
}

// WithParts replaces components (a future, smarter policy plugs in here).
func (p *EligibilityPolicy) WithParts(lc LifecyclePolicy, fr FreshnessPolicy, gx GeographicExpansionPolicy) *EligibilityPolicy {
	cp := *p
	if lc != nil {
		cp.lifecycle = lc
	}
	if fr != nil {
		cp.freshness = fr
	}
	if gx != nil {
		cp.geography = gx
	}
	return &cp
}

// Config returns the policy's parameters.
func (p *EligibilityPolicy) Config() PolicyConfig { return p.cfg }

// CeilingFrom is the expires_at a lifecycle starting at start gets.
func (p *EligibilityPolicy) CeilingFrom(start time.Time) time.Time {
	return p.lifecycle.CeilingFrom(start)
}

// Evaluate is the canonical eligibility decision. Pure and deterministic.
func (p *EligibilityPolicy) Evaluate(f ListingFacts, now time.Time) Eligibility {
	lc := p.lifecycle.Lifecycle(f, now)
	fr := p.freshness.Freshness(f, now)
	gs := p.geography.Scope(f, lc, now)
	e := Eligibility{Status: f.Status, Lifecycle: lc, Freshness: fr, Geography: gs, EvaluatedAt: now,
		Reasons: []string{}, Notes: []string{}}

	switch {
	case f.Status == ListingDraft:
		e.Reasons = append(e.Reasons, "not_published")
	case f.Status == ListingArchived && f.Rejected:
		e.Reasons = append(e.Reasons, "rejected")
	case f.Status != ListingActive:
		e.Reasons = append(e.Reasons, "status_"+string(f.Status))
	}
	if lc.Expired {
		e.Reasons = append(e.Reasons, "lifecycle_expired")
	}
	if f.VendorActive != nil && !*f.VendorActive {
		e.Reasons = append(e.Reasons, "vendor_inactive")
	}
	if f.Product == nil || *f.Product == "" {
		e.Reasons = append(e.Reasons, "no_product")
	}
	if f.Price == nil {
		e.Reasons = append(e.Reasons, "no_price")
	}
	if gs.Stage == GeoPending {
		e.Reasons = append(e.Reasons, "geo_scope_not_started")
	}
	e.Eligible = len(e.Reasons) == 0

	if fr.State != Fresh {
		e.Notes = append(e.Notes, "stale_needs_reconfirmation")
	}
	if !gs.Origin.Resolved {
		e.Notes = append(e.Notes, "origin_unresolved")
	}
	return e
}

// EvaluateListing is Evaluate over a listing.
func (p *EligibilityPolicy) EvaluateListing(l *Listing, vendorActive *bool, now time.Time) Eligibility {
	return p.Evaluate(FactsFromListing(l, vendorActive), now)
}

// ---- deterministic implementations -----------------------------------------

// CeilingLifecycle: the lifecycle starts at publication and ends at the
// persisted ceiling (expires_at). A listing is live for ages [0, ceiling);
// at exactly the ceiling it is expired. The ceiling is fixed when the
// lifecycle starts; verification never moves it.
type CeilingLifecycle struct{ Max time.Duration }

func (c CeilingLifecycle) CeilingFrom(start time.Time) time.Time { return start.Add(c.Max) }

func (c CeilingLifecycle) Lifecycle(f ListingFacts, now time.Time) LifecycleView {
	v := LifecycleView{StartedAt: f.PublishedAt, ExpiresAt: f.ExpiresAt, MaxDays: int(c.Max / day)}
	start := f.CreatedAt
	if f.PublishedAt != nil {
		start = *f.PublishedAt
		if !f.ExpiresAt.IsZero() && f.ExpiresAt.After(start) {
			v.MaxDays = int(f.ExpiresAt.Sub(start) / day)
		}
	}
	if now.After(start) {
		v.Age = now.Sub(start)
	}
	v.Day = int(v.Age / day)
	v.Expired = !f.ExpiresAt.IsZero() && !now.Before(f.ExpiresAt)
	switch {
	case f.Status == ListingSold || f.Status == ListingUnavailable || f.Status == ListingArchived || f.Status == ListingExpired:
		v.State = LifecycleClosed
	case v.Expired:
		v.State = LifecycleExpired
	case f.Status == ListingDraft || f.PublishedAt == nil:
		v.State = LifecycleNotPublished
	default:
		v.State = LifecycleLive
	}
	return v
}

// WindowFreshness: FRESH while the last verification is younger than the
// window (age < window); STALE from exactly the window onwards; UNVERIFIED
// without a verification time.
type WindowFreshness struct{ Window time.Duration }

func (w WindowFreshness) Freshness(f ListingFacts, now time.Time) FreshnessView {
	v := FreshnessView{Window: w.Window, VerifiedAt: f.LastVerifiedAt}
	if f.LastVerifiedAt == nil {
		v.State = Unverified
		return v
	}
	if now.After(*f.LastVerifiedAt) {
		v.Age = now.Sub(*f.LastVerifiedAt)
	}
	v.AgeHours = int(v.Age / time.Hour)
	staleAt := f.LastVerifiedAt.Add(w.Window)
	v.StaleAt = &staleAt
	if v.Age < w.Window {
		v.State = Fresh
	} else {
		v.State = Stale
	}
	return v
}

// ExpansionStage is one step of the staged geographic policy (data, not code).
type ExpansionStage struct {
	Stage  GeoStage
	Scope  string
	After  time.Duration // lifecycle age at which the stage begins
	Levels []geo.Level   // levels eligible during the stage (cumulative)
	Reason string
}

// StagedExpansion: the stage is the last one whose start the lifecycle age
// has reached (a stage begins at exactly its start); EXPIRED from the
// ceiling. Equal starts skip the earlier stage.
type StagedExpansion struct {
	Stages   []ExpansionStage
	Resolver geo.Resolver
}

// NewStagedExpansion is the initial Majo policy. NEARBY means the origin's
// surrounding areas; without adjacency data (deferred) it widens to the
// origin city.
func NewStagedExpansion(cfg PolicyConfig, resolver geo.Resolver) *StagedExpansion {
	origin := []geo.Level{geo.LevelExactArea, geo.LevelCommunity, geo.LevelLGA}
	nearby := append(append([]geo.Level{}, origin...), geo.LevelCity)
	state := append(append([]geo.Level{}, nearby...), geo.LevelState)
	region := append(append([]geo.Level{}, state...), geo.LevelRegion)
	return &StagedExpansion{Resolver: resolver, Stages: []ExpansionStage{
		{GeoLocal, "ORIGIN_AREA", cfg.GeoLocalAfter, origin, "initial lifecycle stage: origin area"},
		{GeoNearby, "NEARBY_AREAS", cfg.GeoNearbyAfter, nearby, "local search window elapsed: nearby areas"},
		{GeoState, "STATE", cfg.GeoStateAfter, state, "nearby search window elapsed: whole state"},
		{GeoRegion, "REGION", cfg.GeoRegionAfter, region, "state search window elapsed: broader region"},
	}}
}

func (s *StagedExpansion) Scope(f ListingFacts, lc LifecycleView, now time.Time) GeoScope {
	g := GeoScope{Stage: GeoNone, Scope: "NONE", Levels: []geo.Level{}}
	if f.Location != nil {
		g.Origin = s.Resolver.Resolve(*f.Location)
	}
	switch lc.State {
	case LifecycleExpired:
		g.Stage, g.Scope, g.Reason = GeoExpired, "NONE", "lifecycle ceiling reached"
		return g
	case LifecycleClosed:
		g.Reason = "listing closed (" + string(f.Status) + ")"
		return g
	case LifecycleNotPublished:
		g.Reason = "not published: the lifecycle has not started"
		return g
	}
	start := *lc.StartedAt
	current := -1
	for i, st := range s.Stages {
		if lc.Age >= st.After {
			current = i
		}
	}
	if current < 0 {
		g.Stage, g.Reason = GeoPending, "local stage starts later in the lifecycle"
	} else {
		st := s.Stages[current]
		g.Stage, g.Scope, g.Reason = st.Stage, st.Scope, st.Reason
		g.Levels = append([]geo.Level{}, st.Levels...)
	}
	// Next change: the next stage that begins before the ceiling, else EXPIRED.
	for _, st := range s.Stages[current+1:] {
		at := start.Add(st.After)
		if at.After(start.Add(lc.Age)) && at.Before(lc.ExpiresAt) {
			g.NextStage, g.NextExpansionAt = st.Stage, &at
			break
		}
	}
	if g.NextExpansionAt == nil {
		at := lc.ExpiresAt
		g.NextStage, g.NextExpansionAt = GeoExpired, &at
	}
	if !g.Origin.Resolved {
		g.Reason += "; origin location not resolved (scope applies to the raw location text)"
	}
	return g
}
