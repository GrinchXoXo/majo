package httpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/whatsapp"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/vendor"
)

type recConvRepo struct{ saved []*conversation.Message }

func (r *recConvRepo) FindOrCreateByExternalID(ctx context.Context, ch, ext string) (*conversation.Conversation, error) {
	return &conversation.Conversation{ID: uuid.New(), Channel: ch, ExternalID: ext}, nil
}
func (r *recConvRepo) FindByID(ctx context.Context, id uuid.UUID) (*conversation.Conversation, error) {
	return nil, nil
}
func (r *recConvRepo) SaveMessage(ctx context.Context, m *conversation.Message) (*conversation.Message, error) {
	m.ID = uuid.New()
	r.saved = append(r.saved, m)
	return m, nil
}
func (r *recConvRepo) MessageExists(ctx context.Context, id uuid.UUID, ext string) (bool, error) {
	return false, nil
}
func (r *recConvRepo) LinkBuyer(ctx context.Context, c, b uuid.UUID) error { return nil }

type activeVendorRepo struct{ v *vendor.Vendor }

func (a activeVendorRepo) Create(ctx context.Context, v *vendor.Vendor) (*vendor.Vendor, error) {
	return v, nil
}
func (a activeVendorRepo) Update(ctx context.Context, id uuid.UUID, f vendor.Fields) (*vendor.Vendor, error) {
	return a.v, nil
}
func (a activeVendorRepo) FindByID(ctx context.Context, id uuid.UUID) (*vendor.Vendor, error) {
	return a.v, nil
}
func (a activeVendorRepo) FindByWhatsAppID(ctx context.Context, id string) (*vendor.Vendor, error) {
	return a.v, nil
}
func (a activeVendorRepo) List(ctx context.Context, s *vendor.Status, c *string) ([]*vendor.Vendor, error) {
	return nil, nil
}
func (a activeVendorRepo) ActiveByCategory(ctx context.Context, c string) ([]*vendor.Vendor, error) {
	return nil, nil
}
func (a activeVendorRepo) RecordMessageActivity(ctx context.Context, id uuid.UUID) error  { return nil }
func (a activeVendorRepo) RecordSupplySubmission(ctx context.Context, id uuid.UUID) error { return nil }
func (a activeVendorRepo) RecordResponseActivity(ctx context.Context, id uuid.UUID) error { return nil }
func (a activeVendorRepo) RecordRequestSent(ctx context.Context, id uuid.UUID) error      { return nil }

type recEvents struct{ all []*event.Event }

func (r *recEvents) Write(ctx context.Context, e *event.Event) error {
	r.all = append(r.all, e)
	return nil
}

const voiceNoteFromVendor = `{"entry":[{"changes":[{"value":{"messages":[{"from":"2348000000001","id":"wamid.V1","timestamp":"1","type":"audio","audio":{"id":"MEDIA-9","mime_type":"audio/ogg; codecs=opus","voice":true}}]}}]}]}`

func newTestWebhook(allowUnsigned bool) (*WhatsAppWebhookHandler, *recConvRepo, *recEvents) {
	conv := &recConvRepo{}
	ev := &recEvents{}
	v := &vendor.Vendor{ID: uuid.New(), BusinessName: "Mike Gadgets", Status: vendor.StatusActive}
	h := NewWhatsAppWebhookHandler(
		whatsapp.NewCloudAPIProvider("", "", "", "verify", "app-secret"),
		conv, nil, nil, activeVendorRepo{v}, ev, nil, nil, nil, "verify", allowUnsigned,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)
	return h, conv, ev
}

func post(h http.Handler, body, signature string) int {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/whatsapp", bytes.NewBufferString(body))
	if signature != "" {
		req.Header.Set("X-Hub-Signature-256", signature)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code
}

func metaSign(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestWebhook_RejectsUnsignedAndForgedPayloads(t *testing.T) {
	h, conv, ev := newTestWebhook(false)

	if code := post(h, voiceNoteFromVendor, ""); code != http.StatusUnauthorized {
		t.Fatalf("unsigned payload: expected 401, got %d", code)
	}
	if code := post(h, voiceNoteFromVendor, metaSign("attacker-guess", voiceNoteFromVendor)); code != http.StatusUnauthorized {
		t.Fatalf("forged payload: expected 401, got %d", code)
	}
	if len(conv.saved) != 0 || len(ev.all) != 0 {
		t.Fatalf("rejected payloads must not be persisted or acted on (saved=%d events=%d)", len(conv.saved), len(ev.all))
	}
}

func TestWebhook_AcceptsValidSignatureAndPersistsMediaRef(t *testing.T) {
	h, conv, ev := newTestWebhook(false)

	if code := post(h, voiceNoteFromVendor, metaSign("app-secret", voiceNoteFromVendor)); code != http.StatusOK {
		t.Fatalf("valid signature: expected 200, got %d", code)
	}
	if len(conv.saved) != 1 {
		t.Fatalf("expected the message to be persisted, got %d", len(conv.saved))
	}
	media, ok := conv.saved[0].Content["media"].(*conversation.MediaRef)
	if !ok || media.Kind != "audio" || media.Provider != "whatsapp" || media.ProviderRef != "MEDIA-9" || !media.Voice {
		t.Fatalf("expected channel-agnostic MediaRef persisted in content, got %#v", conv.saved[0].Content["media"])
	}
	if len(ev.all) != 1 || ev.all[0].ActorType != event.ActorVendor {
		t.Fatalf("expected one MESSAGE_RECEIVED attributed to the VENDOR, got %+v", ev.all)
	}
}

func TestWebhook_DevelopmentModeAllowsUnsignedOnlyWhenExplicit(t *testing.T) {
	h, conv, _ := newTestWebhook(true)
	if code := post(h, voiceNoteFromVendor, ""); code != http.StatusOK || len(conv.saved) != 1 {
		t.Fatalf("AllowUnsignedWebhooks=true should accept unsigned (dev only); code=%d saved=%d", code, len(conv.saved))
	}
}

// --- §23A delivery receipts ---

type fakeDeliveries struct {
	seen map[string]map[string]bool // wamid -> status -> already applied
	meta map[string]map[string]any
}

func (f *fakeDeliveries) ApplyDeliveryStatus(ctx context.Context, wamid, status string, at time.Time, errDetail string) (*conversation.DeliveryTransition, error) {
	meta, ok := f.meta[wamid]
	if !ok {
		return nil, nil
	}
	if f.seen[wamid] == nil {
		f.seen[wamid] = map[string]bool{}
	}
	t := &conversation.DeliveryTransition{Message: &conversation.Message{ID: uuid.New(), Metadata: meta}}
	switch status {
	case "delivered":
		t.FirstDelivered = !f.seen[wamid]["delivered"]
		f.seen[wamid]["delivered"] = true
	case "read":
		t.FirstDelivered = !f.seen[wamid]["delivered"]
		t.FirstRead = !f.seen[wamid]["read"]
		f.seen[wamid]["delivered"], f.seen[wamid]["read"] = true, true
	case "failed":
		t.FirstFailed = !f.seen[wamid]["failed"]
		f.seen[wamid]["failed"] = true
	default:
		return nil, nil
	}
	return t, nil
}

func statusPayload(wamid, status string) string {
	return `{"entry":[{"changes":[{"value":{"statuses":[{"id":"` + wamid + `","status":"` + status +
		`","timestamp":"1759700000","recipient_id":"2348099999999"}]}}]}]}`
}

func TestWebhook_DeliveryReceiptsProduceOneFunnelEventPerTransition(t *testing.T) {
	conv := &recConvRepo{}
	ev := &recEvents{}
	reqID := uuid.New()
	deliveries := &fakeDeliveries{seen: map[string]map[string]bool{}, meta: map[string]map[string]any{
		"wamid.OFFER": {"kind": "offer_delivery", "entity_type": "buyer_request", "entity_id": reqID.String()},
	}}
	h := NewWhatsAppWebhookHandler(
		whatsapp.NewCloudAPIProvider("", "", "", "verify", "app-secret"),
		conv, deliveries, nil, activeVendorRepo{}, ev, nil, nil, nil, "verify", false,
		slog.New(slog.NewTextHandler(io.Discard, nil)),
	)

	for _, st := range []string{"sent", "delivered", "delivered", "read", "read"} {
		body := statusPayload("wamid.OFFER", st)
		if code := post(h, body, metaSign("app-secret", body)); code != http.StatusOK {
			t.Fatalf("status %s: expected 200, got %d", st, code)
		}
	}
	// A receipt for a message Majo never recorded is ignored.
	body := statusPayload("wamid.UNKNOWN", "delivered")
	post(h, body, metaSign("app-secret", body))

	counts := map[event.Type]int{}
	for _, e := range ev.all {
		counts[e.EventType]++
		if e.EntityType != "buyer_request" || e.EntityID == nil || *e.EntityID != reqID || e.Payload["kind"] != "offer_delivery" {
			t.Fatalf("receipt event must be attributed to the offer's buyer request, got %+v", e)
		}
	}
	if counts[event.MessageDelivered] != 1 || counts[event.MessageRead] != 1 || len(ev.all) != 2 {
		t.Fatalf("expected exactly one MESSAGE_DELIVERED and one MESSAGE_READ, got %v", counts)
	}
	if len(conv.saved) != 0 {
		t.Fatal("status callbacks must not be stored as inbound messages")
	}
}

// ---- Phase 4A.1: vendor media hand-off --------------------------------------

type recEnqueuer struct {
	calls []struct {
		vendor, msg uuid.UUID
		ref         *conversation.MediaRef
	}
}

func (r *recEnqueuer) EnqueueDownload(ctx context.Context, vendorID, messageID uuid.UUID, ref *conversation.MediaRef) error {
	r.calls = append(r.calls, struct {
		vendor, msg uuid.UUID
		ref         *conversation.MediaRef
	}{vendorID, messageID, ref})
	return nil
}

const imageFromVendor = `{"entry":[{"changes":[{"value":{"messages":[{"from":"2348000000001","id":"wamid.IMG1","timestamp":"1","type":"image","image":{"id":"MEDIA-IMG-1","mime_type":"image/jpeg","sha256":"abc","caption":"Imported ladies bags ₦18,500"}}]}}]}]}`

func TestWebhook_VendorMediaIsHandedToDurableIngestion(t *testing.T) {
	h, conv, _ := newTestWebhook(false)
	enq := &recEnqueuer{}
	h.SetMediaIngestion(enq)

	if code := post(h, imageFromVendor, metaSign("app-secret", imageFromVendor)); code != http.StatusOK {
		t.Fatalf("expected 200, got %d", code)
	}
	if len(conv.saved) != 1 || len(enq.calls) != 1 {
		t.Fatalf("expected 1 saved message and 1 enqueue, got %d/%d", len(conv.saved), len(enq.calls))
	}
	c := enq.calls[0]
	if c.msg != conv.saved[0].ID || c.ref.ProviderRef != "MEDIA-IMG-1" || c.ref.Kind != "image" || c.ref.Caption != "Imported ladies bags ₦18,500" {
		t.Fatalf("unexpected hand-off: %+v ref=%+v", c, c.ref)
	}
	// The MediaRef persisted on the message is unchanged by 4A.1.
	if ref, ok := conv.saved[0].Content["media"].(*conversation.MediaRef); !ok || ref.ProviderRef != "MEDIA-IMG-1" {
		t.Fatalf("MediaRef not persisted as before: %#v", conv.saved[0].Content["media"])
	}
}

func TestWebhook_RejectedPayloadNeverEnqueuesMedia(t *testing.T) {
	h, _, _ := newTestWebhook(false)
	enq := &recEnqueuer{}
	h.SetMediaIngestion(enq)
	if code := post(h, imageFromVendor, metaSign("wrong", imageFromVendor)); code != http.StatusUnauthorized || len(enq.calls) != 0 {
		t.Fatalf("forged payload must not enqueue: code=%d calls=%d", code, len(enq.calls))
	}
}
