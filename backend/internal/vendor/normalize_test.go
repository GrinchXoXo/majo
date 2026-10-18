package vendor

import (
	"reflect"
	"testing"
)

func TestNormalizeWhatsAppID(t *testing.T) {
	for in, want := range map[string]string{
		"2348031234567": "2348031234567", "+234 803 123 4567": "2348031234567", "08031234567": "2348031234567",
		"0803-123-4567": "2348031234567", "+234 (0) 803 123 4567": "2348031234567", "002348031234567": "2348031234567",
		" 234.803.123.4567 ": "2348031234567", "+44 7700 900123": "447700900123", "8031234567": "2348031234567", "903 123 4567": "2349031234567",
	} {
		if got, err := NormalizeWhatsAppID(in); err != nil || got != want {
			t.Errorf("%q -> %q, %v (want %q)", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "abc", "0803", "080312345678901234", "2348031234567x", "+234 803 123 4567 ext 2", "٠٨٠٣١٢٣٤٥٦٧"} {
		if got, err := NormalizeWhatsAppID(bad); err == nil {
			t.Errorf("%q accepted as %q", bad, got)
		}
	}
}

func TestNormalizeCategories(t *testing.T) {
	out, unknown := NormalizeCategories([]string{" Phones", "FOOD", "food", "", "Small  Chops", "electronics"})
	if !reflect.DeepEqual(out, []string{"electronics", "food", "phones", "small chops"}) || !reflect.DeepEqual(unknown, []string{"small chops"}) {
		t.Fatalf("%v %v", out, unknown)
	}
	if o, u := NormalizeCategories(nil); o != nil || u != nil {
		t.Fatal("nil means unchanged")
	}
	if got := NormalizeAreas([]string{" Akoka ", "akoka", "Moremi  Hall", ""}); !reflect.DeepEqual(got, []string{"Akoka", "Moremi Hall"}) {
		t.Fatal(got)
	}
	if !ValidStatus(StatusActive) || ValidStatus("ACTIV") {
		t.Fatal("status")
	}
}
