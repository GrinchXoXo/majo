package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/channel"
)

// captureServer records every JSON message payload and media upload.
type captureServer struct {
	mu       sync.Mutex
	payloads []map[string]any
	uploads  []string
	fail     bool
	srv      *httptest.Server
}

func newCapture(t *testing.T) *captureServer {
	c := &captureServer{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.fail {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"code":131047,"message":"Re-engagement message"}}`))
			return
		}
		if strings.HasSuffix(r.URL.Path, "/media") {
			if err := r.ParseMultipartForm(1 << 20); err != nil {
				t.Errorf("upload is not multipart: %v", err)
			}
			f, hdr, err := r.FormFile("file")
			if err != nil {
				t.Errorf("no file part: %v", err)
				return
			}
			b, _ := io.ReadAll(f)
			c.uploads = append(c.uploads, r.FormValue("messaging_product")+"|"+r.FormValue("type")+"|"+hdr.Filename+"|"+string(b))
			w.Write([]byte(`{"id":"upl-1"}`))
			return
		}
		var p map[string]any
		_ = json.NewDecoder(r.Body).Decode(&p)
		c.payloads = append(c.payloads, p)
		w.Write([]byte(`{"messages":[{"id":"wamid.rich"}]}`))
	}))
	return c
}

func (c *captureServer) last(t *testing.T) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.payloads) == 0 {
		t.Fatal("nothing sent")
	}
	return c.payloads[len(c.payloads)-1]
}

func jsonOf(v any) string { b, _ := json.Marshal(v); return string(b) }

func TestSendMessage_AllKinds(t *testing.T) {
	c := newCapture(t)
	defer c.srv.Close()
	p := NewCloudAPIProvider(c.srv.URL, "tok", "PNID", "", "")
	ctx := context.Background()
	img := &channel.MediaRef{AssetID: uuid.New(), Kind: "image", ProviderRef: "upl-1"}
	vid := &channel.MediaRef{AssetID: uuid.New(), Kind: "video", ProviderRef: "upl-2"}

	cases := []struct {
		msg  channel.Message
		want []string // substrings of the JSON payload
	}{
		{channel.Message{Kind: channel.KindImage, Media: img, Body: "Ladies bag ₦18,500"},
			[]string{`"type":"image"`, `"image":{"caption":"Ladies bag ₦18,500","id":"upl-1"}`}},
		{channel.Message{Kind: channel.KindVideo, Media: vid, Body: "video"},
			[]string{`"type":"video"`, `"video":{"caption":"video","id":"upl-2"}`}},
		{channel.Message{Kind: channel.KindButtons, Header: img, Body: "card", Footer: "Ref L-1",
			Buttons: []channel.Button{{ID: "dd:x:l:L-1:ask", Title: "Ask about it"}}},
			[]string{`"type":"interactive"`, `"header":{"image":{"id":"upl-1"},"type":"image"}`,
				`"buttons":[{"reply":{"id":"dd:x:l:L-1:ask","title":"Ask about it"},"type":"reply"}]`, `"footer":{"text":"Ref L-1"}`}},
		{channel.Message{Kind: channel.KindList, Body: "finds", ListButton: "See finds", Section: "Today",
			Rows: []channel.Row{{ID: "dd:x:l:L-1", Title: "Bag", Description: "₦18,500"}}},
			[]string{`"type":"list"`, `"button":"See finds"`, `"rows":[{"description":"₦18,500","id":"dd:x:l:L-1","title":"Bag"}]`, `"title":"Today"`}},
		{channel.Message{Kind: channel.KindTemplate, Template: &channel.Template{Name: "majo_finds", Language: "en",
			BodyParams: []string{"3 finds"}, QuickReplyPayloads: []string{"dd:x:show"}}},
			[]string{`"type":"template"`, `"name":"majo_finds"`, `"language":{"code":"en"}`, `{"text":"3 finds","type":"text"}`,
				`"sub_type":"quick_reply"`, `{"payload":"dd:x:show","type":"payload"}`}},
		{channel.Message{Kind: channel.KindText, Body: "hello"}, []string{`"text":{"body":"hello"}`}},
	}
	for _, tc := range cases {
		id, err := p.SendMessage(ctx, "2348000000001", tc.msg)
		if err != nil || id != "wamid.rich" {
			t.Fatalf("%s: %v %q", tc.msg.Kind, err, id)
		}
		got := jsonOf(c.last(t))
		for _, w := range tc.want {
			if !strings.Contains(got, w) {
				t.Errorf("%s payload missing %s\n%s", tc.msg.Kind, w, got)
			}
		}
	}
}

func TestSendMessage_RejectsBeforeSending(t *testing.T) {
	c := newCapture(t)
	defer c.srv.Close()
	p := NewCloudAPIProvider(c.srv.URL, "tok", "PNID", "", "")
	bad := []channel.Message{
		{Kind: channel.KindButtons, Body: "x", Buttons: []channel.Button{{ID: "a", Title: "1"}, {ID: "b", Title: "2"}, {ID: "c", Title: "3"}, {ID: "d", Title: "4"}}},
		{Kind: channel.KindButtons, Body: "x", Buttons: []channel.Button{{ID: "a", Title: "a title that is far too long"}}},
		{Kind: channel.KindImage, Media: &channel.MediaRef{AssetID: uuid.New()}}, // not uploaded
		{Kind: channel.KindTemplate},
	}
	for _, m := range bad {
		if _, err := p.SendMessage(context.Background(), "234", m); !errors.Is(err, channel.ErrInvalidMessage) {
			t.Errorf("%s: want ErrInvalidMessage, got %v", m.Kind, err)
		}
	}
	if len(c.payloads) != 0 {
		t.Fatalf("invalid messages must never reach the provider: %d sent", len(c.payloads))
	}
}

func TestSendMessage_ProviderFailure(t *testing.T) {
	c := newCapture(t)
	defer c.srv.Close()
	c.fail = true
	_, err := NewCloudAPIProvider(c.srv.URL, "tok", "PNID", "", "").SendMessage(context.Background(), "234", channel.Message{Kind: channel.KindText, Body: "x"})
	if err == nil || !strings.Contains(err.Error(), "131047") {
		t.Fatalf("provider error must surface with its code: %v", err)
	}
}

func TestUploadMedia(t *testing.T) {
	c := newCapture(t)
	defer c.srv.Close()
	id, err := NewCloudAPIProvider(c.srv.URL, "tok", "PNID", "", "").UploadMedia(context.Background(), []byte("PNGDATA"), "image/png", "asset.png")
	if err != nil || id != "upl-1" || len(c.uploads) != 1 || c.uploads[0] != "whatsapp|image/png|asset.png|PNGDATA" {
		t.Fatalf("upload: %q %v %v", id, err, c.uploads)
	}
}

func TestParseWebhookPayload_InteractiveReplies(t *testing.T) {
	payload := []byte(`{"entry":[{"changes":[{"value":{"messages":[
		{"from":"234","id":"wamid.b","timestamp":"1","type":"interactive","interactive":{"type":"button_reply","button_reply":{"id":"dd:x:l:L-1:ask","title":"Ask about it"}}},
		{"from":"234","id":"wamid.l","timestamp":"1","type":"interactive","interactive":{"type":"list_reply","list_reply":{"id":"dd:x:l:L-1","title":"Bag","description":"₦1"}}},
		{"from":"234","id":"wamid.q","timestamp":"1","type":"button","button":{"payload":"dd:x:show","text":"Show me"}},
		{"from":"234","id":"wamid.t","timestamp":"1","type":"text","text":{"body":"hi"}}
	]}}]}]}`)
	msgs, err := (&CloudAPIProvider{}).ParseWebhookPayload(payload)
	if err != nil || len(msgs) != 4 {
		t.Fatalf("%v %d", err, len(msgs))
	}
	want := []*Interaction{
		{Kind: "button_reply", ID: "dd:x:l:L-1:ask", Title: "Ask about it"},
		{Kind: "list_reply", ID: "dd:x:l:L-1", Title: "Bag"},
		{Kind: "quick_reply", ID: "dd:x:show", Title: "Show me"},
		nil,
	}
	for i, w := range want {
		got := msgs[i].Interaction
		if (w == nil) != (got == nil) || (w != nil && *got != *w) {
			t.Errorf("message %d: got %+v want %+v", i, got, w)
		}
		if w != nil && msgs[i].Text != w.Title {
			t.Errorf("message %d text %q", i, msgs[i].Text)
		}
	}
}
