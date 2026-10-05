package whatsapp

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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
