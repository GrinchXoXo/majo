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
