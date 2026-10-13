package errortrack

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/getsentry/sentry-go"
)

func TestScrub(t *testing.T) {
	cases := map[string]string{
		"send to 2348012345678 failed":                 "send to [number] failed",
		"vendor +234 801 234 5678 unreachable":         "vendor [number] unreachable",
		"Authorization: Bearer abc.DEF-123_x":          "Authorization: Bearer [redacted]",
		"token majo_op_0123abcdef in log":              "token majo_op_[redacted] in log",
		"mail ada@example.com":                         "mail [email]",
		"meta code 131047 job attempt 3 of 5":          "meta code 131047 job attempt 3 of 5", // short codes survive
		"request 9f1c2a3b-1111-2222-3333-444455556666": "request 9f1c2a3b-1111-2222-3333-444455556666",
	}
	for in, want := range cases {
		if got := Scrub(in); got != want {
			t.Errorf("Scrub(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDisabledWithoutDSN(t *testing.T) {
	flush, err := Init(Config{})
	if err != nil || Enabled() {
		t.Fatalf("empty DSN must leave reporting off: %v", err)
	}
	flush()
	CaptureError(errors.New("x"), nil) // no-op, must not panic
}

// fakeIngest is a minimal Sentry-protocol endpoint (what GlitchTip exposes):
// it records envelope bodies.
type fakeIngest struct {
	mu     sync.Mutex
	bodies []string
}

func (f *fakeIngest) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var rd io.Reader = r.Body
	if r.Header.Get("Content-Encoding") == "gzip" {
		gz, err := gzip.NewReader(r.Body)
		if err == nil {
			rd = gz
		}
	}
	b, _ := io.ReadAll(rd)
	f.mu.Lock()
	f.bodies = append(f.bodies, r.URL.Path+"\n"+string(b))
	f.mu.Unlock()
	w.WriteHeader(200)
}

func (f *fakeIngest) all() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return strings.Join(f.bodies, "\n---\n")
}

func TestReportsAreSentAndScrubbed(t *testing.T) {
	ingest := &fakeIngest{}
	srv := httptest.NewServer(ingest)
	defer srv.Close()
	dsn := strings.Replace(srv.URL, "http://", "http://publickey@", 1) + "/42"
	flush, err := Init(Config{DSN: dsn, Environment: "test", Release: "majo-backend@test"})
	if err != nil || !Enabled() {
		t.Fatalf("init: %v", err)
	}
	defer func() { enabled.Store(false); sentry.Init(sentry.ClientOptions{}) }()

	// 1. ERROR log records are reported, with tags; WARN records are not.
	var buf bytes.Buffer
	log := slog.New(SlogHandler(slog.NewJSONHandler(&buf, nil))).With("service", "majo-test")
	log.Warn("durable job failed, retry scheduled", "error", "temporary")
	log.Error("durable job failed terminally", "error", "send to 2348012345678: 131026 not on WhatsApp",
		"job_type", "VENDOR_REQUEST_RESEND", "auth", "Bearer s3cr3t-token")
	// 2. HTTP panics are recovered, answered 500 and reported.
	h := Recoverer(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom for buyer ada@example.com")
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/api/requests", nil))
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "INTERNAL") {
		t.Fatalf("panic answer: %d %s", rec.Code, rec.Body.String())
	}
	// 3. Direct capture.
	CaptureError(errors.New("matching failed for +234 801 234 5678"), map[string]string{"component": "matching"})
	flush()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && strings.Count(ingest.all(), "/api/42/envelope/") < 3 {
		time.Sleep(50 * time.Millisecond)
	}
	got := ingest.all()
	if n := strings.Count(got, "/api/42/envelope/"); n != 3 {
		t.Fatalf("want 3 envelopes at the Sentry envelope endpoint, got %d:\n%s", n, got)
	}
	for _, want := range []string{"durable job failed terminally", "VENDOR_REQUEST_RESEND", "boom for buyer [email]",
		"matching failed for [number]", `"component":"matching"`, `"environment":"test"`, "majo-backend@test"} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q", want)
		}
	}
	if !strings.Contains(got, `"stacktrace"`) {
		t.Error("stack traces must still be sent")
	}
	for _, leak := range []string{"2348012345678", "234 801 234 5678", "ada@example.com", "s3cr3t-token", "retry scheduled", `"context_line"`, `"vars"`} {
		if strings.Contains(got, leak) {
			t.Errorf("report leaked %q", leak)
		}
	}
	// The log line itself is unchanged (the scrubbing applies to reports only).
	if !strings.Contains(buf.String(), "durable job failed terminally") {
		t.Error("slog record not passed through")
	}
}
