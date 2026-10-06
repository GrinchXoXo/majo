// Package geo is Majo's minimal geography boundary (Phase 4A.5).
//
// It is NOT a GIS: no coordinates, no distances, no polygons, no PostGIS.
// It names geographic levels, describes a place as a set of levels
// (country > region > state > city > LGA > community), and resolves the
// free-text locations Majo stores today (listing/vendor/buyer location) into
// a Place through a Resolver. The resolver is an adapter: today a small,
// deterministic, in-memory table for the pilot; later a real gazetteer or a
// geocoding provider can implement the same interface without touching
// SupplyListing, Matching or Discovery.
//
// Nigeria-first, not Nigeria-only: levels are generic ("STATE" is a
// state/province, "LGA" a district); Nigeria's mapping is data.
package geo

import (
	"strings"
	"unicode"
)

// Level is a geographic level, from narrowest to widest.
type Level string

const (
	LevelExactArea Level = "EXACT_AREA" // the exact place given (street, estate, market)
	LevelCommunity Level = "COMMUNITY"  // neighbourhood / community (e.g. "GRA")
	LevelLGA       Level = "LGA"        // district: Local Government Area in Nigeria
	LevelCity      Level = "CITY"
	LevelState     Level = "STATE"  // state / province
	LevelRegion    Level = "REGION" // e.g. Nigeria's geopolitical zones
	LevelCountry   Level = "COUNTRY"
)

// Levels lists every level, narrowest first.
var Levels = []Level{LevelExactArea, LevelCommunity, LevelLGA, LevelCity, LevelState, LevelRegion, LevelCountry}

// Place is a location described at whichever levels are known. Empty
// fields are unknown, never guessed.
type Place struct {
	Raw       string `json:"raw"`
	Community string `json:"community,omitempty"`
	LGA       string `json:"lga,omitempty"`
	City      string `json:"city,omitempty"`
	State     string `json:"state,omitempty"`
	Region    string `json:"region,omitempty"`
	Country   string `json:"country,omitempty"` // ISO 3166-1 alpha-2
	// Resolved is true when the place was matched to a known city or state.
	Resolved bool `json:"resolved"`
}

// Resolver turns a stored free-text location into a Place. Implementations
// must be deterministic and must not call the network from the eligibility
// path (it runs per listing, per read).
type Resolver interface {
	Resolve(text string) Place
}

// Compact is the comparison form used for locations: lowercase, letters and
// digits only ("Benin City" = "BENIN-CITY" = "benincity"). It matches
// supply.NormalizeSearch.
func Compact(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Gazetteer entry: a city (or a state) and its parents.
type Entry struct {
	Aliases []string // compared in Compact form
	City    string   // empty for a state-only entry
	State   string
	Region  string
	Country string
}

// StaticResolver resolves against a fixed list of entries. The text's last
// comma-separated part that matches an alias decides the place; the parts
// before it are kept as the community ("GRA, Benin City").
type StaticResolver struct {
	byAlias map[string]Entry
}

func NewStaticResolver(entries []Entry) *StaticResolver {
	r := &StaticResolver{byAlias: map[string]Entry{}}
	for _, e := range entries {
		for _, a := range e.Aliases {
			r.byAlias[Compact(a)] = e
		}
	}
	return r
}

func (r *StaticResolver) Resolve(text string) Place {
	p := Place{Raw: strings.TrimSpace(text)}
	if p.Raw == "" {
		return p
	}
	parts := strings.Split(p.Raw, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		e, ok := r.byAlias[Compact(parts[i])]
		if !ok {
			continue
		}
		p.City, p.State, p.Region, p.Country, p.Resolved = e.City, e.State, e.Region, e.Country, true
		if i > 0 {
			p.Community = strings.TrimSpace(strings.Join(parts[:i], ","))
		}
		return p
	}
	return p
}

// NigeriaPilot is the initial gazetteer: only the places the pilot can
// populate reliably (Architecture Lock V2 §O-5 candidate cities and a few
// major ones). It is deliberately NOT a national LGA database; anything
// else resolves as unresolved raw text, which the policy handles.
var NigeriaPilot = []Entry{
	{Aliases: []string{"Benin City", "Benin"}, City: "Benin City", State: "Edo", Region: "South-South", Country: "NG"},
	{Aliases: []string{"Port Harcourt", "PH", "Port-Harcourt"}, City: "Port Harcourt", State: "Rivers", Region: "South-South", Country: "NG"},
	{Aliases: []string{"Warri"}, City: "Warri", State: "Delta", Region: "South-South", Country: "NG"},
	{Aliases: []string{"Lagos", "Ikeja", "Lekki", "Yaba"}, City: "Lagos", State: "Lagos", Region: "South-West", Country: "NG"},
	{Aliases: []string{"Abuja", "FCT"}, City: "Abuja", State: "FCT", Region: "North-Central", Country: "NG"},
	{Aliases: []string{"Edo", "Edo State"}, State: "Edo", Region: "South-South", Country: "NG"},
	{Aliases: []string{"Rivers", "Rivers State"}, State: "Rivers", Region: "South-South", Country: "NG"},
	{Aliases: []string{"Delta", "Delta State"}, State: "Delta", Region: "South-South", Country: "NG"},
}

// Within reports the narrowest level at which the viewer lies inside the
// origin's area, and false when the two places cannot be related (different
// countries, or an unresolved place whose raw text differs). Pure.
//
// The origin's area is the narrowest level the origin was given at: an
// origin "GRA, Benin City" has the community GRA as its area; an origin
// "Benin City" has the whole city as its area; "Edo" the whole state. A
// viewer inside that area is at EXACT_AREA (or COMMUNITY). A viewer described
// less precisely than the origin (e.g. only "Benin City" against an origin
// "GRA, Benin City") is never assumed to be inside the narrower area: it is
// related at the narrowest level both places state (CITY here).
func Within(origin, viewer Place) (Level, bool) {
	if origin.Raw != "" && Compact(origin.Raw) == Compact(viewer.Raw) {
		return LevelExactArea, true
	}
	if !origin.Resolved || !viewer.Resolved {
		return "", false
	}
	same := func(a, b string) bool { return a != "" && b != "" && Compact(a) == Compact(b) }
	sameParent := same(origin.City, viewer.City) || (origin.City == "" && viewer.City == "" && same(origin.State, viewer.State))
	switch {
	case origin.Community != "":
		if same(origin.Community, viewer.Community) && sameParent {
			return LevelCommunity, true
		}
	case origin.LGA != "":
		if same(origin.LGA, viewer.LGA) && same(origin.State, viewer.State) {
			return LevelLGA, true
		}
	case origin.City != "":
		if same(origin.City, viewer.City) {
			return LevelExactArea, true // the origin area IS the city
		}
	case origin.State != "":
		if same(origin.State, viewer.State) {
			return LevelExactArea, true // the origin area IS the state
		}
	}
	if same(origin.LGA, viewer.LGA) && same(origin.State, viewer.State) {
		return LevelLGA, true
	}
	if same(origin.City, viewer.City) {
		return LevelCity, true
	}
	if same(origin.State, viewer.State) {
		return LevelState, true
	}
	if same(origin.Region, viewer.Region) {
		return LevelRegion, true
	}
	if same(origin.Country, viewer.Country) {
		return LevelCountry, true
	}
	return "", false
}
