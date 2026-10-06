package structid

import (
	"testing"

	"github.com/google/uuid"
)

func TestParseRoundTrip(t *testing.T) {
	d := uuid.MustParse("6f1c2a3e-1b2c-4d5e-8f90-a1b2c3d4e5f6")
	cases := map[string]ID{
		Show(d):                {Action: ActionShow, DeliveryID: d},
		Category(d, "phones"):  {Action: ActionCategory, DeliveryID: d, Category: "phones"},
		Listing(d, "L-1042AB"): {Action: ActionListing, DeliveryID: d, ListingRef: "L-1042AB"},
		Video(d, "L-1042AB"):   {Action: ActionVideo, DeliveryID: d, ListingRef: "L-1042AB"},
		Ask(d, "L-1042AB"):     {Action: ActionAsk, DeliveryID: d, ListingRef: "L-1042AB"},
		NotInterested(d):       {Action: ActionNotInterested, DeliveryID: d},
		Stop(d):                {Action: ActionStop, DeliveryID: d},
		ConsentIn("v1"):        {Action: ActionConsentIn, Wording: "v1"},
		ConsentNo():            {Action: ActionConsentNo},
	}
	for raw, want := range cases {
		got, err := Parse(raw)
		want.Raw = raw
		if err != nil || got != want {
			t.Errorf("%q: %+v %v", raw, got, err)
		}
		if !Looks(raw) {
			t.Errorf("%q must look structured", raw)
		}
	}
}

func TestParseRejectsMalformed(t *testing.T) {
	d := uuid.New().String()
	for _, bad := range []string{
		"dd:", "dd:not-a-uuid:show", "dd:00000000-0000-0000-0000-000000000000:show", "dd:" + d,
		"dd:" + d + ":buy", "dd:" + d + ":l:1042", "dd:" + d + ":l:L-1042:delete", "dd:" + d + ":cat:Phones!",
		"dd:" + d + ":show:extra", "dc:in:", "dc:in:V1 ", "dc:yes", "1", "ask", "",
	} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%q must be malformed", bad)
		}
	}
	if Looks("I want 2 phones") || Looks("1") {
		t.Fatal("free text is not structured")
	}
}
