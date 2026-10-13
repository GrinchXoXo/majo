package routing

import (
	"strings"
	"testing"

	"github.com/middleman/backend/internal/autonomy"
)

func TestMatchVendorOption(t *testing.T) {
	opts := []autonomy.Option{
		{Key: "1", Label: "iphone 13 128GB (MM-111111)", Ref: "MM-111111", Value: map[string]any{"label": "iphone 13 128GB (MM-111111)"}},
		{Key: "2", Label: "ps5 (MM-222222)", Ref: "MM-222222", Value: map[string]any{"label": "ps5 (MM-222222)"}},
	}
	for text, want := range map[string]string{"1": "1", "2.": "2", "#2": "2", "MM-222222 yes": "2", "the iphone one": "1", "ps5": "2"} {
		o := MatchVendorOption(text, opts)
		if o == nil || o.Key != want {
			t.Fatalf("%q -> %+v, want %s", text, o, want)
		}
	}
	for _, text := range []string{"3", "which one?", "yes"} {
		if o := MatchVendorOption(text, opts); o != nil {
			t.Fatalf("%q must not match, got %s", text, o.Key)
		}
	}
}

func TestVendorDetailQuestion(t *testing.T) {
	q, _ := VendorDetailQuestion("Which one?", "iphone 13 128GB", nil)
	if !strings.Contains(q, "The buyer is asking for iphone 13 128GB. Do you have that exact item?") {
		t.Fatalf("question reply: %q", q)
	}
	p := int64(620000)
	q, c := VendorDetailQuestion("620k", "iphone 13", &p)
	if !strings.Contains(q, "available at ₦620,000") || c["price"] != int64(620000) {
		t.Fatalf("bare price: %q %+v", q, c)
	}
	q, _ = VendorDetailQuestion("maybe", "ps5", nil)
	if !strings.Contains(q, "Do you have ps5 available?") {
		t.Fatalf("generic: %q", q)
	}
}
