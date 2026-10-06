// Package structid defines Majo's structured interaction IDs (Architecture
// Lock V2 §H), the authoritative form of a buyer's tap on a button, list row
// or template quick reply:
//
//	dd:<delivery_id>:show                 open a discovery delivery
//	dd:<delivery_id>:cat:<category>       browse one category of it
//	dd:<delivery_id>:l:<listing_ref>      open one listing card
//	dd:<delivery_id>:l:<listing_ref>:video  see the listing's video
//	dd:<delivery_id>:l:<listing_ref>:ask  "Ask about it" -> BuyerRequest
//	dd:<delivery_id>:not_interested
//	dd:<delivery_id>:stop                 opt out of discovery
//	dc:in:<wording_version>               discovery consent: opt in
//	dc:no                                 discovery consent: decline
//
// Parse is strict: anything else is malformed and must change no state.
package structid

import (
	"errors"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// Action is what a structured ID asks for.
type Action string

const (
	ActionShow          Action = "show"
	ActionCategory      Action = "category"
	ActionListing       Action = "listing"
	ActionVideo         Action = "video"
	ActionAsk           Action = "ask"
	ActionNotInterested Action = "not_interested"
	ActionStop          Action = "stop"
	ActionConsentIn     Action = "consent_in"
	ActionConsentNo     Action = "consent_no"
)

// ID is a parsed structured interaction ID.
type ID struct {
	Raw        string
	Action     Action
	DeliveryID uuid.UUID // dd:* only
	ListingRef string    // listing / video / ask
	Category   string    // category
	Wording    string    // consent_in
}

// ErrMalformed: the text claims to be a structured ID but does not parse.
var ErrMalformed = errors.New("malformed structured interaction id")

var (
	refPattern      = regexp.MustCompile(`^L-[A-Z0-9]{4,12}$`)
	categoryPattern = regexp.MustCompile(`^[a-z0-9_-]{1,40}$`)
	wordingPattern  = regexp.MustCompile(`^[a-z0-9._-]{1,20}$`)
)

// Looks reports whether text uses the structured-ID namespace at all
// (so it must never fall through to the free-text pipeline).
func Looks(text string) bool {
	t := strings.TrimSpace(text)
	return strings.HasPrefix(t, "dd:") || strings.HasPrefix(t, "dc:")
}

// Parse parses a structured ID strictly.
func Parse(text string) (ID, error) {
	raw := strings.TrimSpace(text)
	id := ID{Raw: raw}
	parts := strings.Split(raw, ":")
	switch {
	case len(parts) >= 2 && parts[0] == "dc":
		switch {
		case len(parts) == 3 && parts[1] == "in" && wordingPattern.MatchString(parts[2]):
			id.Action, id.Wording = ActionConsentIn, parts[2]
			return id, nil
		case len(parts) == 2 && parts[1] == "no":
			id.Action = ActionConsentNo
			return id, nil
		}
		return id, ErrMalformed
	case len(parts) >= 3 && parts[0] == "dd":
		did, err := uuid.Parse(parts[1])
		if err != nil || did == uuid.Nil {
			return id, ErrMalformed
		}
		id.DeliveryID = did
		rest := parts[2:]
		switch {
		case len(rest) == 1 && rest[0] == "show":
			id.Action = ActionShow
		case len(rest) == 1 && rest[0] == "not_interested":
			id.Action = ActionNotInterested
		case len(rest) == 1 && rest[0] == "stop":
			id.Action = ActionStop
		case len(rest) == 2 && rest[0] == "cat" && categoryPattern.MatchString(rest[1]):
			id.Action, id.Category = ActionCategory, rest[1]
		case len(rest) == 2 && rest[0] == "l" && refPattern.MatchString(rest[1]):
			id.Action, id.ListingRef = ActionListing, rest[1]
		case len(rest) == 3 && rest[0] == "l" && refPattern.MatchString(rest[1]) && rest[2] == "video":
			id.Action, id.ListingRef = ActionVideo, rest[1]
		case len(rest) == 3 && rest[0] == "l" && refPattern.MatchString(rest[1]) && rest[2] == "ask":
			id.Action, id.ListingRef = ActionAsk, rest[1]
		default:
			return id, ErrMalformed
		}
		return id, nil
	}
	return id, ErrMalformed
}

// Builders (the only way presentation creates IDs).
func Show(d uuid.UUID) string               { return "dd:" + d.String() + ":show" }
func Category(d uuid.UUID, c string) string { return "dd:" + d.String() + ":cat:" + c }
func Listing(d uuid.UUID, ref string) string {
	return "dd:" + d.String() + ":l:" + ref
}
func Video(d uuid.UUID, ref string) string { return Listing(d, ref) + ":video" }
func Ask(d uuid.UUID, ref string) string   { return Listing(d, ref) + ":ask" }
func NotInterested(d uuid.UUID) string     { return "dd:" + d.String() + ":not_interested" }
func Stop(d uuid.UUID) string              { return "dd:" + d.String() + ":stop" }
func ConsentIn(wording string) string      { return "dc:in:" + wording }
func ConsentNo() string                    { return "dc:no" }
