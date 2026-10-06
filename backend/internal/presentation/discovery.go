// Package presentation renders Majo intents into channel-neutral messages
// (Phase 4B, Architecture Lock V2 §H). It decides text, image, video,
// buttons, lists, templates and the structured IDs on them; the channel
// adapter only transports. Distribution never builds payloads itself.
package presentation

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/channel"
	"github.com/middleman/backend/internal/structid"
)

// Version identifies this presentation (recorded on each DiscoveryBatch).
const Version = "presentation-v1"

// Arm is a deterministic presentation variant (pilot comparison, not an
// experimentation platform).
type Arm string

const (
	// ArmA: image + caption + "Ask about it".
	ArmA Arm = "ARM_A"
	// ArmB: image + caption + "Watch video" (when the listing has video) + "Ask about it".
	ArmB Arm = "ARM_B"
)

// KnownArm reports whether a is implemented.
func KnownArm(a Arm) bool { return a == ArmA || a == ArmB }

// AssignArm picks an arm deterministically from (batch, buyer): the same
// pair always gets the same arm, with no stored randomness.
func AssignArm(batchID, buyerID uuid.UUID, arms []Arm) Arm {
	if len(arms) == 0 {
		return ArmA
	}
	h := fnv.New32a()
	h.Write(batchID[:])
	h.Write(buyerID[:])
	return arms[h.Sum32()%uint32(len(arms))]
}

// Card is the buyer-safe view of one listing: no vendor name, number or
// internal IDs; the public L- reference is the only identity shown.
type Card struct {
	Ref                 string
	Product             string
	Category            string
	Price               *int64
	Condition           string
	Area                string
	Image               *uuid.UUID // stored MediaAsset (primary image)
	Video               *uuid.UUID // stored MediaAsset (video)
	NeedsReconfirmation bool
}

// TemplateConfig is the approved discovery template (configuration).
type TemplateConfig struct {
	Name     string
	Language string
}

// ErrTemplateRequired: the buyer is outside the 24-hour customer-service
// window and no approved template is configured [PLATFORM].
var ErrTemplateRequired = errors.New("outside the 24-hour window and no discovery template is configured")

// maxListedCards leaves room for the two control rows in a 10-row list.
const maxListedCards = channel.MaxRows - 2

// Entry is the first message(s) of a discovery delivery. Inside the 24-hour
// window the finds are shown directly; outside it only the configured
// template may be sent, with one quick reply that opens the delivery.
func Entry(delivery uuid.UUID, arm Arm, cards []Card, sessionOpen bool, tpl TemplateConfig) ([]channel.Message, error) {
	if len(cards) == 0 {
		return nil, errors.New("no cards to present")
	}
	if !sessionOpen {
		if tpl.Name == "" || tpl.Language == "" {
			return nil, ErrTemplateRequired
		}
		return []channel.Message{{Kind: channel.KindTemplate, Template: &channel.Template{
			Name: tpl.Name, Language: tpl.Language, BodyParams: []string{Summary(cards)},
			QuickReplyPayloads: []string{structid.Show(delivery)},
		}}}, nil
	}
	return Overview(delivery, arm, cards), nil
}

// Summary is the one-line description used in the template body.
func Summary(cards []Card) string {
	if len(cards) == 1 {
		return cards[0].Product
	}
	return fmt.Sprintf("%d finds, including %s", len(cards), cards[0].Product)
}

// Overview: one card directly, or a list of finds with control rows.
func Overview(delivery uuid.UUID, arm Arm, cards []Card) []channel.Message {
	if len(cards) == 1 {
		return []channel.Message{CardMessage(delivery, arm, cards[0])}
	}
	if len(cards) > maxListedCards {
		cards = cards[:maxListedCards]
	}
	rows := make([]channel.Row, 0, len(cards)+2)
	for _, c := range cards {
		rows = append(rows, channel.Row{ID: structid.Listing(delivery, c.Ref), Title: clip(c.Product, channel.MaxRowTitle),
			Description: clip(strings.Join(nonEmpty(Price(c.Price), c.Area), " · "), channel.MaxRowDescription)})
	}
	rows = append(rows,
		channel.Row{ID: structid.NotInterested(delivery), Title: "Not interested"},
		channel.Row{ID: structid.Stop(delivery), Title: "Stop these messages"})
	return []channel.Message{{Kind: channel.KindList, Body: fmt.Sprintf("I found %d things you might like 👀 Tap to see each one.", len(cards)),
		ListButton: "See finds", Section: "Majo finds", Rows: rows}}
}

// CardMessage is one listing: primary image header (when stored), caption
// and the arm's buttons.
func CardMessage(delivery uuid.UUID, arm Arm, c Card) channel.Message {
	m := channel.Message{Kind: channel.KindButtons, Body: Caption(c)}
	if c.Image != nil {
		m.Header = &channel.MediaRef{AssetID: *c.Image, Kind: "image"}
	}
	m.Buttons = append(m.Buttons, channel.Button{ID: structid.Ask(delivery, c.Ref), Title: "Ask about it"})
	if arm == ArmB && c.Video != nil {
		m.Buttons = append(m.Buttons, channel.Button{ID: structid.Video(delivery, c.Ref), Title: "Watch video"})
	}
	m.Buttons = append(m.Buttons, channel.Button{ID: structid.NotInterested(delivery), Title: "Not interested"})
	return m
}

// TextOnly drops the image header (used when a card's media cannot be
// published, so one listing's media failure never blanks the delivery).
func TextOnly(m channel.Message) channel.Message {
	m.Header = nil
	return m
}

// VideoMessages: the stored video, then an "Ask about it" prompt.
func VideoMessages(delivery uuid.UUID, c Card) []channel.Message {
	if c.Video == nil {
		return []channel.Message{{Kind: channel.KindText, Body: "Sorry, there's no video for " + c.Product + " yet."}}
	}
	return []channel.Message{
		{Kind: channel.KindVideo, Media: &channel.MediaRef{AssetID: *c.Video, Kind: "video"}, Body: c.Product + " · Ref " + c.Ref},
		{Kind: channel.KindButtons, Body: "Interested in " + c.Product + "?", Buttons: []channel.Button{
			{ID: structid.Ask(delivery, c.Ref), Title: "Ask about it"},
			{ID: structid.NotInterested(delivery), Title: "Not interested"},
		}},
	}
}

// Caption is the buyer-facing listing text. A stale listing is never shown
// as guaranteed availability (canonical freshness, Phase 4A.5).
func Caption(c Card) string {
	var sb strings.Builder
	sb.WriteString("*" + c.Product + "*")
	if line := strings.Join(nonEmpty(Price(c.Price), c.Condition), " · "); line != "" {
		sb.WriteString("\n" + line)
	}
	if c.Area != "" {
		sb.WriteString("\n📍 " + c.Area)
	}
	sb.WriteString("\nRef " + c.Ref)
	if c.NeedsReconfirmation {
		sb.WriteString("\n_Availability to be confirmed with the seller._")
	}
	return sb.String()
}

// AskAck answers "Ask about it": interest is recorded, nothing is promised.
func AskAck(c Card) string {
	return "Great choice 👍 I'm checking with the seller that " + c.Product + " (Ref " + c.Ref +
		") is still available. I'll send you the offer as soon as they confirm.\nNothing has been paid or reserved yet."
}

// Fixed replies.
const (
	NotInterestedAck   = "Got it 👍 I'll show you fewer things like that."
	StopAck            = "Done ✅ You won't get Majo finds anymore. You can still message me any time to look for something."
	ExpiredInteraction = "Sorry, that option has expired. Tell me what you're looking for and I'll search 👀"
	ConsentInAck       = "You're in ✅ I'll send you occasional finds that match what you like. Reply STOP any time to stop."
	ConsentNoAck       = "No problem 👍 I won't send you finds. You can still message me any time to look for something."
	NotSubscribed      = "You're not receiving Majo finds, so there's nothing to stop 👍"
)

// ConsentPrompt asks for explicit discovery opt-in (wording is versioned).
func ConsentPrompt(wordingVersion string) channel.Message {
	return channel.Message{Kind: channel.KindButtons,
		Body: "Would you like Majo to send you occasional finds from trusted sellers near you? " +
			"You can stop any time by replying STOP.",
		Buttons: []channel.Button{
			{ID: structid.ConsentIn(wordingVersion), Title: "Yes, send finds"},
			{ID: structid.ConsentNo(), Title: "No thanks"},
		}}
}

// Price formats a naira amount ("₦18,500").
func Price(p *int64) string {
	if p == nil {
		return ""
	}
	s := strconv.FormatInt(*p, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var out []byte
	for i := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, s[i])
	}
	if neg {
		return "-₦" + string(out)
	}
	return "₦" + string(out)
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

func nonEmpty(xs ...string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}
