package routing

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/channel"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/vendor"
)

// scriptedText: SendText returns err (nil = accepted) and records the via tag.
type scriptedText struct {
	err  error
	sent []string
	via  []string
}

func (s *scriptedText) SendText(ctx context.Context, to, body string) error {
	s.sent = append(s.sent, body)
	if p, ok := conversation.OutboundPurposeFrom(ctx); ok {
		s.via = append(s.via, p.Extra["sent_as"].(string))
	}
	return s.err
}

type fakeRich struct {
	err  error
	msgs []channel.Message
	via  []string
}

func (f *fakeRich) SendRecorded(ctx context.Context, to string, m channel.Message) (*uuid.UUID, string, error) {
	f.msgs = append(f.msgs, m)
	if p, ok := conversation.OutboundPurposeFrom(ctx); ok {
		f.via = append(f.via, p.Extra["sent_as"].(string))
		if p.Kind != "vendor_request" || p.EntityID == nil {
			panic("template send lost its purpose")
		}
	}
	return nil, "wamid.T", f.err
}

type fakeWindow struct {
	at  *time.Time
	err error
}

func (f fakeWindow) LastInboundAt(context.Context, string, string) (*time.Time, error) {
	return f.at, f.err
}

var windowRefused = &channel.ProviderError{Provider: "whatsapp", HTTPStatus: 400, Code: 131047, Message: "Re-engagement message"}

func routeOnce(t *testing.T, text *scriptedText, rich *fakeRich, window InboundWindow, tplName string) *VendorRequest {
	t.Helper()
	routingRepo := newFakeRoutingRepo()
	vendorRepo := newFakeVendorRepo()
	v, _ := vendorRepo.Create(context.Background(), &vendor.Vendor{BusinessName: "Chops", WhatsAppExternalID: "2348000000009", Status: vendor.StatusActive})
	svc := NewService(vendorRepo, &fakeSupplyRepo{}, routingRepo, &confirmedRequestRepo{}, &fakeAI{}, &fakeEvents{}, &fakeAudits{}, text, testLogger())
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	svc.SetVendorTemplate(VendorTemplate{Name: tplName}, rich, window)
	req, _ := (&confirmedRequestRepo{}).FindByID(context.Background(), uuid.New())
	created, err := svc.RouteToVendors(context.Background(), req, []uuid.UUID{v.ID}, nil, nil)
	if err != nil || len(created) != 1 {
		t.Fatalf("route: %v %d", err, len(created))
	}
	return created[0]
}

func ago(d time.Duration) *time.Time {
	t := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC).Add(-d)
	return &t
}

func TestVendorTemplate_DisabledKeepsFreeText(t *testing.T) {
	text, rich := &scriptedText{}, &fakeRich{}
	vr := routeOnce(t, text, rich, fakeWindow{}, "")
	if len(text.sent) != 1 || len(rich.msgs) != 0 || vr.Status != VendorRequestSent || text.via[0] != "text" {
		t.Fatalf("no template configured: free text as before (text=%d rich=%d)", len(text.sent), len(rich.msgs))
	}
}

func TestVendorTemplate_WindowOpenSendsText(t *testing.T) {
	text, rich := &scriptedText{}, &fakeRich{}
	vr := routeOnce(t, text, rich, fakeWindow{at: ago(2 * time.Hour)}, "majo_vendor_request")
	if len(text.sent) != 1 || len(rich.msgs) != 0 || vr.Status != VendorRequestSent {
		t.Fatalf("vendor wrote 2h ago: free text")
	}
}

func TestVendorTemplate_WindowClosedOrUnknownSendsTemplate(t *testing.T) {
	for name, w := range map[string]fakeWindow{
		"closed":     {at: ago(30 * time.Hour)},
		"near edge":  {at: ago(23*time.Hour + 45*time.Minute)},
		"never":      {},
		"lookup err": {err: errors.New("db down")},
	} {
		text, rich := &scriptedText{}, &fakeRich{}
		vr := routeOnce(t, text, rich, w, "majo_vendor_request")
		if len(text.sent) != 0 || len(rich.msgs) != 1 || vr.Status != VendorRequestSent || rich.via[0] != "template" {
			t.Fatalf("%s: expected one template send (text=%d rich=%d status=%s)", name, len(text.sent), len(rich.msgs), vr.Status)
		}
		m := rich.msgs[0]
		if m.Kind != channel.KindTemplate || m.Template.Name != "majo_vendor_request" || m.Template.Language != "en" || len(m.Template.BodyParams) != 4 {
			t.Fatalf("%s: template %+v", name, m.Template)
		}
		if m.Template.BodyParams[3] != vr.PublicReference {
			t.Fatalf("%s: reference param %q != %q", name, m.Template.BodyParams[3], vr.PublicReference)
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("%s: template message invalid: %v", name, err)
		}
	}
}

func TestVendorTemplate_TextRefusedForWindowRetriesAsTemplate(t *testing.T) {
	text, rich := &scriptedText{err: windowRefused}, &fakeRich{}
	vr := routeOnce(t, text, rich, fakeWindow{at: ago(time.Hour)}, "majo_vendor_request")
	if len(text.sent) != 1 || len(rich.msgs) != 1 || vr.Status != VendorRequestSent {
		t.Fatalf("131047 on free text: one template retry (text=%d rich=%d status=%s)", len(text.sent), len(rich.msgs), vr.Status)
	}
}

func TestVendorTemplate_OtherTextFailureIsNotRetriedAsTemplate(t *testing.T) {
	invalid := &channel.ProviderError{Provider: "whatsapp", HTTPStatus: 400, Code: 131026, Message: "undeliverable"}
	text, rich := &scriptedText{err: invalid}, &fakeRich{}
	vr := routeOnce(t, text, rich, fakeWindow{at: ago(time.Hour)}, "majo_vendor_request")
	if len(rich.msgs) != 0 || vr.Status != VendorRequestSendFailed {
		t.Fatalf("a wrong number is not a window problem: no template (rich=%d status=%s)", len(rich.msgs), vr.Status)
	}
}

func TestVendorTemplate_TemplateFailureIsSendFailed(t *testing.T) {
	text, rich := &scriptedText{}, &fakeRich{err: &channel.ProviderError{Provider: "whatsapp", HTTPStatus: 400, Code: 132001, Message: "template does not exist"}}
	vr := routeOnce(t, text, rich, fakeWindow{}, "majo_vendor_request")
	if vr.Status != VendorRequestSendFailed || vr.SendError == nil {
		t.Fatalf("a refused template is a visible SEND_FAILED: %s", vr.Status)
	}
}

func TestVendorTemplateParams(t *testing.T) {
	p, v, c, loc := "small chops", "jumbo\npack", "new", "Benin  City"
	lo, hi := int64(35000), int64(40000)
	q := 3
	req := &request.BuyerRequest{Product: &p, Variant: &v, Condition: &c, Location: &loc, BudgetMin: &lo, BudgetMax: &hi,
		Quantity: &q, Preferences: []string{"for 20 people", "  "}}
	got := VendorTemplateParams("MM-123456", req, nil)
	want := []string{"small chops, jumbo pack, new, x3, for 20 people", "₦35,000 to ₦40,000", "Benin City", "MM-123456"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("param %d: %q want %q", i+1, got[i], want[i])
		}
	}
	empty := VendorTemplateParams("MM-1", &request.BuyerRequest{}, nil)
	if empty[0] != "unspecified item" || empty[1] != "Not stated" || empty[2] != "Not stated" {
		t.Fatalf("never an empty variable: %q", empty)
	}
	for _, s := range append(got, empty...) {
		if s == "" || strings.ContainsAny(s, "\n\t") || strings.Contains(s, "     ") {
			t.Fatalf("Meta rejects %q", s)
		}
	}
	long := strings.Repeat("abcdefghij", 30)
	if r := []rune(VendorTemplateParams("MM-1", &request.BuyerRequest{Product: &long}, nil)[0]); len(r) > 160 {
		t.Fatalf("item capped at 160, got %d", len(r))
	}
	lp, price := "iphone 13", int64(620000)
	l := &supply.Listing{PublicReference: "L-ABC123", Product: &lp, Price: &price}
	lg := VendorTemplateParams("MM-2", &request.BuyerRequest{}, l)
	if lg[0] != "your listing L-ABC123: iphone 13" || lg[1] != "Your listed price ₦620,000" {
		t.Fatalf("listing reconfirmation params: %q", lg)
	}
	max := int64(45000)
	if b := VendorTemplateParams("MM-3", &request.BuyerRequest{BudgetMax: &max}, nil)[1]; b != "Up to ₦45,000" {
		t.Fatalf("budget: %q", b)
	}
}

func TestFormatNaira(t *testing.T) {
	for v, want := range map[int64]string{0: "₦0", 999: "₦999", 1000: "₦1,000", 45000: "₦45,000", 1250000: "₦1,250,000"} {
		if got := FormatNaira(v); got != want {
			t.Fatalf("%d: %q want %q", v, got, want)
		}
	}
}
