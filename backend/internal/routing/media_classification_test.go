package routing

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/ai"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/supply"
	"github.com/middleman/backend/internal/vendor"
)

// Phase 4A.2: deterministic vendor media classification.

type productRequestRepo struct {
	fakeRequestRepo
	product string
}

func (p *productRequestRepo) FindByID(ctx context.Context, id uuid.UUID) (*request.BuyerRequest, error) {
	return &request.BuyerRequest{ID: id, Product: strp(p.product)}, nil
}

type recIntake struct {
	texts, media []supply.Inbound
	reasons      []string
}

func (r *recIntake) SubmitText(ctx context.Context, vendorID uuid.UUID, ref supply.Inbound, text string) error {
	r.texts = append(r.texts, ref)
	return nil
}
func (r *recIntake) Window() time.Duration { return 120 * time.Second }
func (r *recIntake) SubmitMedia(ctx context.Context, vendorID uuid.UUID, ref supply.Inbound, caption, reason string, ext *ai.SupplyExtractionResult) error {
	r.media = append(r.media, ref)
	r.reasons = append(r.reasons, reason)
	return nil
}

func i64(v int64) *int64 { return &v }

// heuristicAI mimics the Python supply heuristics for the test captions.
type heuristicAI struct{ fakeAI }

func (h *heuristicAI) ExtractSupply(ctx context.Context, text string) (*ai.SupplyExtractionResult, error) {
	r := &ai.SupplyExtractionResult{AvailabilityStatus: "AVAILABLE"}
	switch text {
	case "Imported ladies bags ₦18,500", "Imported bags ₦18,500":
		r.Product, r.Price, r.Category = strp("bag"), i64(18500), strp("fashion")
	case "iPhone 13 128GB 620k":
		r.Product, r.Price = strp("iphone 13"), i64(620000)
	case "nice one 620k":
		r.Price = i64(620000)
	}
	return r, nil
}

func classifier(t *testing.T, active int, requestedProduct string) (*Service, *vendor.Vendor, *recIntake) {
	t.Helper()
	routingRepo := newFakeRoutingRepo()
	vendorRepo := newFakeVendorRepo()
	svc := NewService(vendorRepo, &fakeSupplyRepo{}, routingRepo, &productRequestRepo{product: requestedProduct},
		&heuristicAI{}, &fakeEvents{}, &fakeAudits{}, &fakeMessaging{}, testLogger())
	in := &recIntake{}
	svc.SetSupplyIntake(in)
	v := &vendor.Vendor{ID: uuid.New(), WhatsAppExternalID: "234801", Status: vendor.StatusActive}
	vendorRepo.vendors[v.ID] = v
	for i := 0; i < active; i++ {
		sent := time.Now().Add(-time.Hour)
		_, _ = routingRepo.CreateVendorRequest(context.Background(), &VendorRequest{VendorID: v.ID, BuyerRequestID: uuid.New(),
			PublicReference: "MM-10000" + string(rune('0'+i)), Status: VendorRequestSent, SentAt: &sent})
	}
	return svc, v, in
}

func TestClassifyVendorMedia_Rules(t *testing.T) {
	cases := []struct {
		name    string
		active  int
		caption string
		supply  bool
		reason  string
	}{
		{"no active request, no caption", 0, "", true, MediaSupplyNoActiveRequest},
		{"no active request, any caption", 0, "Yes", true, MediaSupplyNoActiveRequest},
		{"caption cites a reference", 0, "MM-123456 here", false, mediaNotSupplyReference},
		{"active request, uncaptioned photo", 1, "", false, mediaNotSupplyNoCaption},
		{"active request, 'Yes' caption (scenario 5)", 1, "Yes", false, mediaNotSupplyReply},
		{"active request, reply with price", 1, "Yes boss, I get am for 620k", false, mediaNotSupplyReply},
		{"active request, price but no product", 1, "nice one 620k", false, mediaNotSupplyIncomplete},
		{"active request, same product as requested", 1, "iPhone 13 128GB 620k", false, mediaNotSupplySameProduct},
		{"active request, clearly unrelated supply (scenario 4)", 1, "Imported ladies bags ₦18,500", true, MediaSupplyUnrelatedCaption},
		{"two active requests, unrelated supply", 2, "Imported bags ₦18,500", true, MediaSupplyUnrelatedCaption},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			svc, v, _ := classifier(t, c.active, "iphone 13")
			ok, reason, _ := svc.ClassifyVendorMedia(context.Background(), v, time.Now(), c.caption)
			if ok != c.supply || reason != c.reason {
				t.Fatalf("got (%v, %s), want (%v, %s)", ok, reason, c.supply, c.reason)
			}
		})
	}
}

func TestProcessVendorMedia_OnlySupplyContentReachesIntake(t *testing.T) {
	svc, v, in := classifier(t, 1, "iphone 13")
	ref := supply.Inbound{MessageID: uuid.New()}
	svc.ProcessVendorMedia(context.Background(), v, ref, "image", "Yes")
	svc.ProcessVendorMedia(context.Background(), v, supply.Inbound{MessageID: uuid.New()}, "audio", "Imported bags ₦18,500")
	if len(in.media) != 0 {
		t.Fatal("ambiguous media and audio must not become supply candidates")
	}
	svc.ProcessVendorMedia(context.Background(), v, ref, "image", "Imported ladies bags ₦18,500")
	if len(in.media) != 1 || in.reasons[0] != MediaSupplyUnrelatedCaption {
		t.Fatalf("expected one candidate, got %+v", in)
	}
}

func TestProcessVendorMessageFrom_TextSupplyGoesToIntakeAndRepliesDoNot(t *testing.T) {
	// No active request: the text is supply -> intake, not the legacy path.
	svc, v, in := classifier(t, 0, "")
	supplies := svc.supplies.(*fakeSupplyRepo)
	svc.ProcessVendorMessageFrom(context.Background(), v, supply.Inbound{MessageID: uuid.New()}, "iPhone 13 128GB, 620k")
	if len(in.texts) != 1 || len(supplies.created) != 0 {
		t.Fatalf("expected intake to receive the text, got intake=%d legacy=%d", len(in.texts), len(supplies.created))
	}
	// Active request: the same kind of text is a VendorResponse (scenario 3).
	svc2, v2, in2 := classifier(t, 1, "iphone 13")
	svc2.aiProvider = &fakeAI{vendorResponseResult: &ai.VendorResponseExtractionResult{Status: "AVAILABLE", ReviewState: "REVIEWED"}}
	resp := svc2.ProcessVendorMessageFrom(context.Background(), v2, supply.Inbound{MessageID: uuid.New()}, "Yes boss, I get am for 620k")
	if resp == nil || resp.VendorRequestID == nil || len(in2.texts) != 0 {
		t.Fatalf("a reply to an active request must stay a VendorResponse, got resp=%v intake=%d", resp, len(in2.texts))
	}
}

// §8-§9 of the step-2 prompt: right after the vendor answered a request,
// follow-up photos/texts stay with the reply unless clearly unrelated.
func TestReplyContext_FollowUpsAfterAReplyAreNotSupply(t *testing.T) {
	svc, v, in := classifier(t, 1, "iphone 13")
	svc.aiProvider = &heuristicAI{fakeAI{vendorResponseResult: &ai.VendorResponseExtractionResult{Status: "AVAILABLE", ReviewState: "REVIEWED"}}}
	events := svc.events.(*fakeEvents)
	// The vendor answers the request; it is no longer SENT.
	if resp := svc.ProcessVendorMessageFrom(context.Background(), v, supply.Inbound{MessageID: uuid.New(), At: time.Now()}, "Yes boss, I get am"); resp == nil || resp.VendorRequestID == nil {
		t.Fatal("expected the reply to attach to the request")
	}
	now := time.Now()
	if ok, reason, _ := svc.ClassifyVendorMedia(context.Background(), v, now, ""); ok || reason != mediaNotSupplyAfterReply {
		t.Fatalf("uncaptioned photo after a reply must stay with the reply, got %v %s", ok, reason)
	}
	svc.ProcessVendorMessageFrom(context.Background(), v, supply.Inbound{MessageID: uuid.New(), At: now}, "nice one 620k")
	if len(in.texts) != 0 {
		t.Fatal("'₦620k' right after a reply must not become supply")
	}
	unresolved := 0
	for _, e := range events.events {
		if e.EventType == "VENDOR_MESSAGE_UNRESOLVED" {
			unresolved++
		}
	}
	if unresolved != 1 {
		t.Fatalf("expected one VENDOR_MESSAGE_UNRESOLVED, got %d", unresolved)
	}
	// Clearly unrelated supply is still supply.
	if ok, reason, _ := svc.ClassifyVendorMedia(context.Background(), v, now, "Imported ladies bags ₦18,500"); !ok || reason != MediaSupplyUnrelatedCaption {
		t.Fatalf("unrelated supply after a reply should be supply, got %v %s", ok, reason)
	}
	// Outside the window the reply no longer frames the message.
	if ok, reason, _ := svc.ClassifyVendorMedia(context.Background(), v, now.Add(10*time.Minute), ""); !ok || reason != MediaSupplyNoActiveRequest {
		t.Fatalf("10 minutes later a photo is proactive supply, got %v %s", ok, reason)
	}
}
