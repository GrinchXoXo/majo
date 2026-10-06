package whatsapp_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/middleman/backend/infrastructure/whatsapp"
	"github.com/middleman/backend/infrastructure/whatsapp/whatsapptest"
)

const token = "test-access-token-SECRET"

var jpeg = append([]byte{0xFF, 0xD8, 0xFF, 0xE0}, []byte("fixture-jpeg-bytes")...)

func setup(t *testing.T) (*whatsapptest.FakeGraph, *whatsapp.CloudAPIProvider) {
	t.Helper()
	g := whatsapptest.New(token)
	t.Cleanup(g.Close)
	return g, whatsapp.NewCloudAPIProvider(g.URL(), token, "PHONE", "verify", "secret")
}

func permanent(err error) bool {
	var me *whatsapp.MediaError
	return errors.As(err, &me) && me.Permanent()
}

func TestDownloadMedia_SuccessReturnsExactBytes(t *testing.T) {
	g, p := setup(t)
	g.AddMedia("media-123", jpeg, "image/jpeg")
	data, mime, err := p.DownloadMedia(context.Background(), "media-123", 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	want := sha256.Sum256(jpeg)
	if hex.EncodeToString(sum[:]) != hex.EncodeToString(want[:]) || mime != "image/jpeg" {
		t.Fatalf("got %d bytes mime=%q", len(data), mime)
	}
	if m, d := g.Calls("media-123"); m != 1 || d != 1 {
		t.Fatalf("expected one metadata + one download call, got %d/%d", m, d)
	}
	for _, h := range g.AuthHeaders() {
		if h != "Bearer "+token {
			t.Fatalf("unexpected auth header %q", h)
		}
	}
}

func TestDownloadMedia_ErrorClassification(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(g *whatsapptest.FakeGraph)
		permanent bool
	}{
		{"metadata 404 (media gone/expired)", func(g *whatsapptest.FakeGraph) { g.FailMetadata("m", 404) }, true},
		{"unknown media id", func(g *whatsapptest.FakeGraph) {}, true},
		{"metadata 400 invalid id", func(g *whatsapptest.FakeGraph) { g.FailMetadata("m", 400) }, true},
		{"metadata 401 auth", func(g *whatsapptest.FakeGraph) { g.FailMetadata("m", 401) }, false},
		{"metadata 403 access", func(g *whatsapptest.FakeGraph) { g.FailMetadata("m", 403) }, false},
		{"metadata 429 rate limit", func(g *whatsapptest.FakeGraph) { g.FailMetadata("m", 429) }, false},
		{"metadata 500", func(g *whatsapptest.FakeGraph) { g.FailMetadata("m", 500) }, false},
		{"metadata 502", func(g *whatsapptest.FakeGraph) { g.FailMetadata("m", 502) }, false},
		{"metadata 503", func(g *whatsapptest.FakeGraph) { g.FailMetadata("m", 503) }, false},
		{"download 503", func(g *whatsapptest.FakeGraph) { g.FailDownload("m", 503) }, false},
		{"download url lapsed 404", func(g *whatsapptest.FakeGraph) { g.FailDownload("m", 404) }, false},
		{"malformed metadata json", func(g *whatsapptest.FakeGraph) { g.MalformedMetadata("m", "{not json") }, true},
		{"metadata without url", func(g *whatsapptest.FakeGraph) { g.MalformedMetadata("m", `{"id":"m"}`) }, true},
		{"declared size over limit", func(g *whatsapptest.FakeGraph) { g.DeclareSize("m", 10<<20) }, true},
		{"truncated body", func(g *whatsapptest.FakeGraph) { g.TruncateOnce("m", 5) }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g, p := setup(t)
			if c.name != "unknown media id" {
				g.AddMedia("m", jpeg, "image/jpeg")
			}
			c.setup(g)
			_, _, err := p.DownloadMedia(context.Background(), "m", 1<<20)
			if err == nil {
				t.Fatal("expected an error")
			}
			if permanent(err) != c.permanent {
				t.Fatalf("permanent=%v, want %v (err: %v)", permanent(err), c.permanent, err)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatal("error message leaks the access token")
			}
		})
	}
}

func TestDownloadMedia_ActualBodyOverLimitIsPermanent(t *testing.T) {
	g, p := setup(t)
	big := append([]byte{0xFF, 0xD8, 0xFF}, make([]byte, 4096)...)
	g.AddMedia("big", big, "image/jpeg")
	g.DeclareSize("big", 10) // provider under-declares; the stream is still bounded
	_, _, err := p.DownloadMedia(context.Background(), "big", 1024)
	if err == nil || !permanent(err) {
		t.Fatalf("expected permanent size error, got %v", err)
	}
}

func TestDownloadMedia_RetryAfterTransientSucceeds(t *testing.T) {
	g, p := setup(t)
	g.AddMedia("m", jpeg, "image/jpeg")
	g.FailMetadata("m", 503, 503)
	for i := 0; i < 2; i++ {
		if _, _, err := p.DownloadMedia(context.Background(), "m", 1<<20); err == nil || permanent(err) {
			t.Fatalf("attempt %d: expected retryable error, got %v", i+1, err)
		}
	}
	if data, _, err := p.DownloadMedia(context.Background(), "m", 1<<20); err != nil || len(data) != len(jpeg) {
		t.Fatalf("third attempt: %v", err)
	}
}

func TestDownloadMedia_RejectsPathLikeMediaIDs(t *testing.T) {
	_, p := setup(t)
	for _, id := range []string{"", "../x", "a/b", "a?b"} {
		if _, _, err := p.DownloadMedia(context.Background(), id, 1024); err == nil || !permanent(err) {
			t.Fatalf("id %q: expected permanent rejection, got %v", id, err)
		}
	}
}

func TestDownloadMedia_TimeoutIsRetryable(t *testing.T) {
	g, p := setup(t)
	g.AddMedia("slow", jpeg, "image/jpeg")
	g.Delay("slow", 2*time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, _, err := p.DownloadMedia(ctx, "slow", 1<<20)
	if err == nil || permanent(err) {
		t.Fatalf("expected a retryable timeout error, got %v", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("the download did not respect the deadline")
	}
}
