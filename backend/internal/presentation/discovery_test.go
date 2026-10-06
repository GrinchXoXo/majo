package presentation

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/channel"
	"github.com/middleman/backend/internal/structid"
)

func ptr[T any](v T) *T { return &v }

func card(ref string, video bool) Card {
	c := Card{Ref: ref, Product: "Imported ladies bag", Category: "fashion", Price: ptr(int64(18500)), Condition: "New",
		Area: "GRA, Benin City", Image: ptr(uuid.New())}
	if video {
		c.Video = ptr(uuid.New())
	}
	return c
}

func mustValid(t *testing.T, ms []channel.Message) {
	t.Helper()
	for _, m := range ms {
		if err := m.Validate(); err != nil {
			t.Fatalf("%s invalid: %v", m.Kind, err)
		}
	}
}

func TestCard_ArmsAndMedia(t *testing.T) {
	d := uuid.New()
	a := CardMessage(d, ArmA, card("L-AAAA01", true))
	b := CardMessage(d, ArmB, card("L-AAAA01", true))
	bNoVideo := CardMessage(d, ArmB, card("L-AAAA01", false))
	mustValid(t, []channel.Message{a, b, bNoVideo})
	if a.Header == nil || a.Header.Kind != "image" {
		t.Fatal("the primary image is the card header")
	}
	ids := func(m channel.Message) string { return strings.Join(m.InteractionIDs(), " ") }
	if strings.Contains(ids(a), ":video") || !strings.Contains(ids(a), structid.Ask(d, "L-AAAA01")) {
		t.Fatalf("ARM_A: image + caption + ask: %s", ids(a))
	}
	if !strings.Contains(ids(b), structid.Video(d, "L-AAAA01")) {
		t.Fatalf("ARM_B with video: video button: %s", ids(b))
	}
	if strings.Contains(ids(bNoVideo), ":video") {
		t.Fatal("ARM_B without video has no video button")
	}
	if !strings.Contains(a.Body, "₦18,500") || !strings.Contains(a.Body, "Ref L-AAAA01") || strings.Contains(a.Body, "confirmed") {
		t.Fatalf("caption: %q", a.Body)
	}
	stale := card("L-AAAA01", false)
	stale.NeedsReconfirmation = true
	if !strings.Contains(CardMessage(d, ArmA, stale).Body, "Availability to be confirmed") {
		t.Fatal("stale listings are never presented as guaranteed availability")
	}
	noImage := card("L-AAAA01", false)
	noImage.Image = nil
	if m := CardMessage(d, ArmA, noImage); m.Header != nil || m.Validate() != nil {
		t.Fatal("no image: text card")
	}
}

func TestEntry_SessionVsTemplate(t *testing.T) {
	d := uuid.New()
	cards := []Card{card("L-AAAA01", false), card("L-BBBB02", true)}
	ms, err := Entry(d, ArmA, cards, true, TemplateConfig{})
	if err != nil || len(ms) != 1 || ms[0].Kind != channel.KindList || len(ms[0].Rows) != 4 {
		t.Fatalf("session: list of finds + 2 control rows: %+v %v", ms, err)
	}
	mustValid(t, ms)
	if ms[0].Rows[3].ID != structid.Stop(d) || ms[0].Rows[2].ID != structid.NotInterested(d) {
		t.Fatalf("control rows: %+v", ms[0].Rows)
	}
	if _, err := Entry(d, ArmA, cards, false, TemplateConfig{}); !errors.Is(err, ErrTemplateRequired) {
		t.Fatalf("outside the window without a template: %v", err)
	}
	ms, err = Entry(d, ArmA, cards, false, TemplateConfig{Name: "majo_finds", Language: "en"})
	if err != nil || ms[0].Kind != channel.KindTemplate || ms[0].Template.QuickReplyPayloads[0] != structid.Show(d) {
		t.Fatalf("template: %+v %v", ms, err)
	}
	mustValid(t, ms)
	one, _ := Entry(d, ArmA, cards[:1], true, TemplateConfig{})
	if one[0].Kind != channel.KindButtons {
		t.Fatal("a single find is shown as its card")
	}
	var many []Card
	for i := 0; i < 15; i++ {
		many = append(many, card("L-CCCC"+string(rune('A'+i))+"0", false))
	}
	ms, _ = Entry(d, ArmA, many, true, TemplateConfig{})
	mustValid(t, ms)
}

func TestVideoMessages(t *testing.T) {
	d := uuid.New()
	ms := VideoMessages(d, card("L-AAAA01", true))
	mustValid(t, ms)
	if ms[0].Kind != channel.KindVideo || ms[0].Media == nil || ms[1].Buttons[0].ID != structid.Ask(d, "L-AAAA01") {
		t.Fatalf("video then ask: %+v", ms)
	}
	if ms := VideoMessages(d, card("L-AAAA01", false)); ms[0].Kind != channel.KindText {
		t.Fatal("no video: text")
	}
}

func TestAssignArmIsDeterministic(t *testing.T) {
	b, arms := uuid.New(), []Arm{ArmA, ArmB}
	seen := map[Arm]int{}
	for i := 0; i < 200; i++ {
		buyer := uuid.NewSHA1(uuid.NameSpaceOID, []byte{byte(i)})
		a := AssignArm(b, buyer, arms)
		if AssignArm(b, buyer, arms) != a {
			t.Fatal("same pair, same arm")
		}
		seen[a]++
	}
	if seen[ArmA] == 0 || seen[ArmB] == 0 {
		t.Fatalf("both arms used: %v", seen)
	}
}

func TestPrice(t *testing.T) {
	for in, want := range map[int64]string{0: "₦0", 999: "₦999", 18500: "₦18,500", 1250000: "₦1,250,000"} {
		if got := Price(&in); got != want {
			t.Errorf("%d: %s", in, got)
		}
	}
}

func TestConsentPrompt(t *testing.T) {
	m := ConsentPrompt("v1")
	mustValid(t, []channel.Message{m})
	if m.Buttons[0].ID != "dc:in:v1" || m.Buttons[1].ID != "dc:no" {
		t.Fatalf("%+v", m.Buttons)
	}
}
