package whatsapp

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"testing"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestVerifyWebhookSignature(t *testing.T) {
	body := []byte(`{"entry":[]}`)
	p := NewCloudAPIProvider("", "", "", "", "app-secret")

	if !p.VerifyWebhookSignature(body, sign("app-secret", body)) {
		t.Fatal("valid Meta signature rejected")
	}
	cases := map[string]string{
		"missing header":          "",
		"wrong secret":            sign("other-secret", body),
		"no sha256= prefix":       sign("app-secret", body)[len("sha256="):],
		"not hex":                 "sha256=zzzz",
		"signature of other body": sign("app-secret", []byte(`{"entry":[{}]}`)),
	}
	for name, header := range cases {
		if p.VerifyWebhookSignature(body, header) {
			t.Errorf("%s: forged/invalid signature accepted", name)
		}
	}

	noSecret := NewCloudAPIProvider("", "", "", "", "")
	if noSecret.VerifyWebhookSignature(body, sign("", body)) {
		t.Fatal("verification must fail closed when no App Secret is configured")
	}
}

func TestParseWebhookPayload_TextMediaAndProfileName(t *testing.T) {
	payload := []byte(`{"entry":[{"changes":[{"value":{
		"contacts":[{"wa_id":"2348011111111","profile":{"name":"Ada"}}],
		"messages":[
			{"from":"2348011111111","id":"wamid.T","timestamp":"1","type":"text","text":{"body":"I need iPhone 13"}},
			{"from":"2348011111111","id":"wamid.A","timestamp":"2","type":"audio","audio":{"id":"MEDIA-1","mime_type":"audio/ogg; codecs=opus","sha256":"abc","voice":true}},
			{"from":"2348011111111","id":"wamid.I","timestamp":"3","type":"image","image":{"id":"MEDIA-2","mime_type":"image/jpeg","caption":"this one"}},
			{"from":"2348011111111","id":"wamid.D","timestamp":"4","type":"document","document":{"id":"MEDIA-3","mime_type":"application/pdf","filename":"quote.pdf"}}
		]}}]}]}`)
	msgs, err := NewCloudAPIProvider("", "", "", "", "").ParseWebhookPayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(msgs))
	}
	if msgs[0].Text != "I need iPhone 13" || msgs[0].Media != nil || msgs[0].ProfileName != "Ada" {
		t.Errorf("text message parsed wrong: %+v", msgs[0])
	}
	if m := msgs[1].Media; m == nil || m.Kind != "audio" || m.ID != "MEDIA-1" || !m.Voice || m.MimeType != "audio/ogg; codecs=opus" {
		t.Errorf("voice note media parsed wrong: %+v", msgs[1].Media)
	}
	if m := msgs[2].Media; m == nil || m.Kind != "image" || m.Caption != "this one" {
		t.Errorf("image media parsed wrong: %+v", msgs[2].Media)
	}
	if m := msgs[3].Media; m == nil || m.Kind != "document" || m.Filename != "quote.pdf" {
		t.Errorf("document media parsed wrong: %+v", msgs[3].Media)
	}
}

func TestSendTextWithID_ReturnsMetaMessageID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/PNID/messages" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("unexpected request %s %s", r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Write([]byte(`{"messaging_product":"whatsapp","contacts":[{"input":"234","wa_id":"234"}],"messages":[{"id":"wamid.HBgN123"}]}`))
	}))
	defer srv.Close()
	id, err := NewCloudAPIProvider(srv.URL, "tok", "PNID", "", "").SendTextWithID(context.Background(), "234", "hi")
	if err != nil || id != "wamid.HBgN123" {
		t.Fatalf("expected wamid.HBgN123, got %q err=%v", id, err)
	}
}

func TestParseStatusUpdates(t *testing.T) {
	payload := []byte(`{"entry":[{"changes":[{"value":{"statuses":[
		{"id":"wamid.1","status":"delivered","timestamp":"1759700000","recipient_id":"234"},
		{"id":"wamid.2","status":"failed","timestamp":"1759700001","recipient_id":"235","errors":[{"code":131047,"title":"Re-engagement message"}]}
	]}}]}]}`)
	got, err := NewCloudAPIProvider("", "", "", "", "").ParseStatusUpdates(payload)
	if err != nil || len(got) != 2 {
		t.Fatalf("expected 2 statuses, got %d err=%v", len(got), err)
	}
	if got[0].MessageID != "wamid.1" || got[0].Status != "delivered" || got[0].Timestamp != "1759700000" {
		t.Errorf("delivered status parsed wrong: %+v", got[0])
	}
	if got[1].Status != "failed" || got[1].ErrorCode != 131047 || got[1].ErrorTitle != "Re-engagement message" {
		t.Errorf("failed status parsed wrong: %+v", got[1])
	}
}
