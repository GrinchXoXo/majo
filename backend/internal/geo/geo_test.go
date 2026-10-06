package geo

import "testing"

func TestStaticResolver(t *testing.T) {
	r := NewStaticResolver(NigeriaPilot)
	cases := map[string]Place{
		"Benin City":             {Raw: "Benin City", City: "Benin City", State: "Edo", Region: "South-South", Country: "NG", Resolved: true},
		"GRA, Benin City":        {Raw: "GRA, Benin City", Community: "GRA", City: "Benin City", State: "Edo", Region: "South-South", Country: "NG", Resolved: true},
		"BENIN-CITY":             {Raw: "BENIN-CITY", City: "Benin City", State: "Edo", Region: "South-South", Country: "NG", Resolved: true},
		"Rumuola, Port Harcourt": {Raw: "Rumuola, Port Harcourt", Community: "Rumuola", City: "Port Harcourt", State: "Rivers", Region: "South-South", Country: "NG", Resolved: true},
		"Edo State":              {Raw: "Edo State", State: "Edo", Region: "South-South", Country: "NG", Resolved: true},
		"Ogbomoso market":        {Raw: "Ogbomoso market"},
		"  ":                     {},
	}
	for in, want := range cases {
		if got := r.Resolve(in); got != want {
			t.Errorf("%q: got %+v want %+v", in, got, want)
		}
	}
}

func TestCompact(t *testing.T) {
	if Compact("Port-Harcourt ") != "portharcourt" || Compact("ÀBUJA") != "àbuja" {
		t.Fatal(Compact("Port-Harcourt "))
	}
}

func TestWithin(t *testing.T) {
	r := NewStaticResolver(NigeriaPilot)
	cases := []struct {
		origin, viewer string
		level          Level
		ok             bool
	}{
		{"GRA, Benin City", "GRA, Benin City", LevelExactArea, true},
		{"GRA, Benin City", "G.R.A., Benin", LevelCommunity, true}, // same community, other spelling/alias
		{"GRA, Benin City", "Ugbowo, Benin City", LevelCity, true},
		{"GRA, Benin City", "Benin City", LevelCity, true}, // less precise viewer: never assumed inside GRA
		{"Benin City", "Ugbowo, Benin City", LevelExactArea, true},
		{"Benin City", "Edo", LevelState, true},
		{"GRA, Benin City", "Edo State", LevelState, true},
		{"GRA, Benin City", "Warri", LevelRegion, true},
		{"GRA, Benin City", "Rumuola, Port Harcourt", LevelRegion, true},
		{"GRA, Benin City", "Lagos", LevelCountry, true},
		{"Edo", "Benin City", LevelExactArea, true},
		{"Ogbomoso market", "Ogbomoso market", LevelExactArea, true},
		{"Ogbomoso market", "Benin City", "", false},
		{"GRA, Benin City", "", "", false},
	}
	for _, c := range cases {
		lv, ok := Within(r.Resolve(c.origin), r.Resolve(c.viewer))
		if lv != c.level || ok != c.ok {
			t.Errorf("Within(%q, %q) = %s,%v want %s,%v", c.origin, c.viewer, lv, ok, c.level, c.ok)
		}
	}
}
