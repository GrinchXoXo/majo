package vendor

import (
	"errors"
	"sort"
	"strings"
)

// ErrBadWhatsAppNumber: not a usable WhatsApp number.
var ErrBadWhatsAppNumber = errors.New("whatsapp number must be a full phone number, e.g. 2348031234567, +234 803 123 4567 or 08031234567")

// NormalizeWhatsAppID turns a phone number as an operator types it into
// the form WhatsApp reports for incoming messages (wa_id): digits only, with
// the country code and no "+". Nigerian local numbers ("0803 123 4567")
// get 234. Without this, a vendor saved as "+234 803..." or "0803..." is
// never recognised: their replies are treated as a buyer's messages (01a
// audit).
func NormalizeWhatsAppID(s string) (string, error) {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= '0' && r <= '9': // ASCII only: WhatsApp IDs never carry other digits
			b.WriteRune(r)
		case r == '+' || r == ' ' || r == '-' || r == '(' || r == ')' || r == '.':
		default:
			return "", ErrBadWhatsAppNumber
		}
	}
	d := b.String()
	if strings.HasPrefix(d, "00") {
		d = d[2:]
	}
	if len(d) == 11 && strings.HasPrefix(d, "0") { // Nigerian local format
		d = "234" + d[1:]
	}
	if len(d) == 10 && strings.ContainsRune("789", rune(d[0])) { // "803 123 4567": Nigerian, no 0 or 234
		d = "234" + d
	}
	if strings.HasPrefix(d, "2340") { // +234 (0)803...
		d = "234" + d[4:]
	}
	if len(d) < 10 || len(d) > 15 || strings.HasPrefix(d, "0") {
		return "", ErrBadWhatsAppNumber
	}
	return d, nil
}

// KnownCategories are the categories buyer requests are sorted into (the AI
// service's vocabulary) plus the finer electronics tags matching groups
// under "electronics". A vendor category outside this list never matches a
// request.
var KnownCategories = []string{"electronics", "phones", "laptops", "gaming", "tv", "computers", "accessories",
	"fashion", "furniture", "food", "services", "school"}

// NormalizeCategories lower-cases, trims and de-duplicates, and reports the
// categories no buyer request will ever carry.
func NormalizeCategories(in []string) (out, unknown []string) {
	if in == nil {
		return nil, nil
	}
	known := map[string]bool{}
	for _, c := range KnownCategories {
		known[c] = true
	}
	seen := map[string]bool{}
	out = []string{}
	for _, c := range in {
		c = strings.ToLower(strings.Join(strings.Fields(c), " "))
		if c == "" || seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
		if !known[c] {
			unknown = append(unknown, c)
		}
	}
	sort.Strings(out)
	return out, unknown
}

// NormalizeAreas trims and de-duplicates (case-insensitively) place names.
func NormalizeAreas(in []string) []string {
	if in == nil {
		return nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, a := range in {
		a = strings.Join(strings.Fields(a), " ")
		if a == "" || seen[strings.ToLower(a)] {
			continue
		}
		seen[strings.ToLower(a)] = true
		out = append(out, a)
	}
	return out
}

// ValidStatus reports whether s is a vendor status.
func ValidStatus(s Status) bool {
	switch s {
	case StatusPending, StatusActive, StatusInactive, StatusBlocked:
		return true
	}
	return false
}
