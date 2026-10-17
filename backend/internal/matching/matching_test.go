package matching

import (
	"testing"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/vendor"
)

func strPtr(s string) *string { return &s }
func i64Ptr(i int64) *int64   { return &i }

// TestMatch_PRD2SmokeTestScenario reproduces the exact scenario from
// PRD 2 §66: a confirmed request for a used iPhone 13 128GB up to ₦650k
// in Benin City, against three vendors. Mike Gadgets (has exact matching
// supply) must win strongly; OJ Phones (right category, no current
// supply) should still appear as a weaker candidate; Furniture World
// must be excluded entirely (no relevant category).
func TestMatch_PRD2SmokeTestScenario(t *testing.T) {
	req := &request.BuyerRequest{
		Category:  strPtr("electronics"),
		Product:   strPtr("iPhone 13"),
		Variant:   strPtr("128GB"),
		Condition: strPtr("used"),
		BudgetMax: i64Ptr(650000),
		Location:  strPtr("Benin City"),
	}

	mikeGadgets := &vendor.Vendor{
		ID:              uuid.New(),
		BusinessName:    "Mike Gadgets",
		Status:          vendor.StatusActive,
		Categories:      []string{"electronics", "phones"},
		PrimaryLocation: "Benin City",
	}
	ojPhones := &vendor.Vendor{
		ID:              uuid.New(),
		BusinessName:    "OJ Phones",
		Status:          vendor.StatusActive,
		Categories:      []string{"phones"},
		PrimaryLocation: "Benin City",
	}
	furnitureWorld := &vendor.Vendor{
		ID:              uuid.New(),
		BusinessName:    "Furniture World",
		Status:          vendor.StatusActive,
		Categories:      []string{"furniture"},
		PrimaryLocation: "Benin City",
	}

	price := int64(620000)
	mikeSupply := &supply.Signal{
		VendorID:           mikeGadgets.ID,
		Product:            strPtr("iPhone 13"),
		NormalizedProduct:  strPtr("iphone13"),
		Variant:            strPtr("128GB"),
		Condition:          strPtr("used"),
		Price:              &price,
		AvailabilityStatus: supply.AvailabilityAvailable,
	}

	supplyByVendor := map[string][]*supply.Signal{
		mikeGadgets.ID.String(): {mikeSupply},
	}

	candidates := Match(
		req,
		[]*vendor.Vendor{mikeGadgets, ojPhones, furnitureWorld},
		supplyByVendor,
		nil,
		DefaultWeights(),
		DefaultMinRelevanceScore,
		DefaultMaxCandidates,
	)

	// Furniture World must be excluded entirely (PRD 2 §17).
	for _, c := range candidates {
		if c.Vendor.BusinessName == "Furniture World" {
			t.Fatal("Furniture World must be excluded — no relevant category")
		}
	}

	if len(candidates) == 0 {
		t.Fatal("expected at least one candidate")
	}

	top := candidates[0]
	if top.Vendor.BusinessName != "Mike Gadgets" {
		t.Fatalf("expected Mike Gadgets to rank first, got %s", top.Vendor.BusinessName)
	}
	if top.MatchType != MatchSupplyConfirmed {
		t.Errorf("expected SUPPLY_CONFIRMED match type, got %s", top.MatchType)
	}
	if top.Score < 90 {
		t.Errorf("expected Mike Gadgets to score highly (exact product+variant+fresh+location+condition), got %d", top.Score)
	}
	if top.WithinBudget == nil || !*top.WithinBudget {
		t.Error("expected Mike Gadgets' ₦620k price to be flagged within the ₦650k budget")
	}

	// OJ Phones should still be a candidate (category relevant) but
	// scored lower since it has no matching supply signal.
	foundOJ := false
	for _, c := range candidates {
		if c.Vendor.BusinessName == "OJ Phones" {
			foundOJ = true
			if c.Score >= top.Score {
				t.Error("expected OJ Phones to score lower than Mike Gadgets")
			}
		}
	}
	if !foundOJ {
		t.Error("expected OJ Phones to appear as a category-relevant candidate")
	}
}

func TestMatch_NoPaddingWhenFewCandidates(t *testing.T) {
	req := &request.BuyerRequest{Category: strPtr("electronics"), Product: strPtr("iPhone 13")}

	onlyVendor := &vendor.Vendor{
		ID:         uuid.New(),
		Status:     vendor.StatusActive,
		Categories: []string{"electronics"},
	}

	candidates := Match(req, []*vendor.Vendor{onlyVendor}, map[string][]*supply.Signal{}, nil, DefaultWeights(), DefaultMinRelevanceScore, DefaultMaxCandidates)

	if len(candidates) > 1 {
		t.Errorf("expected at most 1 candidate (no padding), got %d", len(candidates))
	}
}

func TestMatch_InactiveVendorExcluded(t *testing.T) {
	req := &request.BuyerRequest{Category: strPtr("electronics"), Product: strPtr("iPhone 13")}

	inactive := &vendor.Vendor{
		ID:         uuid.New(),
		Status:     vendor.StatusInactive,
		Categories: []string{"electronics"},
	}

	candidates := Match(req, []*vendor.Vendor{inactive}, map[string][]*supply.Signal{}, nil, DefaultWeights(), DefaultMinRelevanceScore, DefaultMaxCandidates)

	if len(candidates) != 0 {
		t.Errorf("expected inactive vendor to be excluded, got %d candidates", len(candidates))
	}
}

func TestMatch_DeterministicTieBreaking(t *testing.T) {
	req := &request.BuyerRequest{Category: strPtr("electronics")}

	v1 := &vendor.Vendor{ID: uuid.MustParse("00000000-0000-0000-0000-000000000001"), Status: vendor.StatusActive, Categories: []string{"electronics"}}
	v2 := &vendor.Vendor{ID: uuid.MustParse("00000000-0000-0000-0000-000000000002"), Status: vendor.StatusActive, Categories: []string{"electronics"}}

	run1 := Match(req, []*vendor.Vendor{v2, v1}, map[string][]*supply.Signal{}, nil, DefaultWeights(), 0, DefaultMaxCandidates)
	run2 := Match(req, []*vendor.Vendor{v1, v2}, map[string][]*supply.Signal{}, nil, DefaultWeights(), 0, DefaultMaxCandidates)

	if len(run1) != 2 || len(run2) != 2 {
		t.Fatalf("expected 2 candidates in both runs, got %d and %d", len(run1), len(run2))
	}
	if run1[0].Vendor.ID != run2[0].Vendor.ID {
		t.Error("expected identical ranking regardless of input order (deterministic tie-breaking)")
	}
}

// 01a audit: one place named more or less precisely is the same place.
func TestLocationMatches(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"Unilag", "unilag", true}, {"Unilag", "Unilag main gate", true}, {"Yaba, Lagos", "Yaba", true},
		{"GRA, Benin City", "GRA", true}, {"Benin", "Benin City", true}, {"Moremi Hall", "moremi  hall", true},
		{"Unilag", "Akoka", false}, {"Ring Road", "Ikorodu Road", false}, {"Lagos", "Benin City", false},
		{"", "Lagos", false}, {"ui", "Guinness", false}, {"Ikeja", "Ikeja GRA", true},
	} {
		if got := LocationMatches(c.a, c.b); got != c.want {
			t.Errorf("LocationMatches(%q, %q) = %v", c.a, c.b, got)
		}
	}
}
