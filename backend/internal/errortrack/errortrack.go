// Package errortrack sends application errors to a Sentry-compatible
// error tracker (reuse-audit R1 pilot: GlitchTip, self-hosted, or any service
// that accepts the Sentry protocol).
//
// It is off unless SENTRY_DSN is set. When on:
//   - every ERROR-level slog record is also reported (one place, so job
//     failures, recovery failures and provider errors are all covered);
//   - HTTP handler panics are recovered, reported and answered with a 500.
//
// Majo's state is never written here: the tracker only receives error
// reports. Reports are scrubbed before they leave the process: digit runs that
// look like phone numbers, bearer/operator tokens and e-mail addresses are
// masked, and request data (headers, bodies, cookies, IPs) is not sent. If the
// tracker is unreachable the SDK drops the report; Majo carries on.
package errortrack

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/getsentry/sentry-go"
)

// Config for Init.
type Config struct {
	DSN         string
	Environment string
	Release     string
	ServerName  string
}

var enabled atomic.Bool

// Enabled reports whether reports are being sent.
func Enabled() bool { return enabled.Load() }

// Init configures the tracker. With an empty DSN it does nothing and every
// function in this package is a no-op. The returned flush waits (bounded) for
// queued reports at shutdown.
func Init(cfg Config) (flush func(), err error) {
	if strings.TrimSpace(cfg.DSN) == "" {
		return func() {}, nil
	}
	release := cfg.Release
	if release == "" {
		release = "majo-backend@unknown" // never let the SDK shell out to derive one
	}
	err = sentry.Init(sentry.ClientOptions{
		Dsn:              cfg.DSN,
		Environment:      cfg.Environment,
		Release:          release,
		ServerName:       cfg.ServerName,
		AttachStacktrace: true,
		MaxBreadcrumbs:   -1, // breadcrumbs could carry message text
		BeforeSend:       beforeSend,
	})
	if err != nil {
		return func() {}, fmt.Errorf("errortrack: %w", err)
	}
	enabled.Store(true)
	return func() { sentry.Flush(3 * time.Second) }, nil
}

var (
	reDigits = regexp.MustCompile(`\+?\d[\d\s-]{6,}\d`)
	reBearer = regexp.MustCompile(`(?i)bearer\s+[A-Za-z0-9._~+/=-]+`)
	reOpTok  = regexp.MustCompile(`majo_op_[0-9a-f]+`)
	reEmail  = regexp.MustCompile(`[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`)
	reUUID   = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
)

// Scrub masks values that must not leave Majo in an error report: phone
// numbers and other long digit runs, bearer and operator tokens, e-mails.
// Record IDs (UUIDs) are kept: they identify rows, not people.
func Scrub(s string) string {
	s = reOpTok.ReplaceAllString(s, "majo_op_[redacted]")
	s = reBearer.ReplaceAllString(s, "Bearer [redacted]")
	s = reEmail.ReplaceAllString(s, "[email]")
	ids := reUUID.FindAllString(s, -1)
	s = reUUID.ReplaceAllString(s, "\x00")
	s = reDigits.ReplaceAllString(s, "[number]")
	for _, id := range ids {
		s = strings.Replace(s, "\x00", id, 1)
	}
	return s
}

func beforeSend(e *sentry.Event, _ *sentry.EventHint) *sentry.Event {
	e.Message = Scrub(e.Message)
	for i := range e.Threads {
		if st := e.Threads[i].Stacktrace; st != nil {
			for j := range st.Frames {
				st.Frames[j].Vars = nil
				st.Frames[j].PreContext, st.Frames[j].ContextLine, st.Frames[j].PostContext = nil, "", nil
			}
		}
	}
	for i := range e.Exception {
		e.Exception[i].Value = Scrub(e.Exception[i].Value)
		if st := e.Exception[i].Stacktrace; st != nil {
			for j := range st.Frames { // locals and source lines can hold message text
				st.Frames[j].Vars = nil
				st.Frames[j].PreContext, st.Frames[j].ContextLine, st.Frames[j].PostContext = nil, "", nil
			}
		}
	}
	for _, c := range e.Contexts {
		for k, v := range c {
			if sv, ok := v.(string); ok {
				c[k] = Scrub(sv)
			}
		}
	}
	for k, v := range e.Tags {
		e.Tags[k] = Scrub(v)
	}
	e.Breadcrumbs = nil
	e.Request = nil
	e.User = sentry.User{}
	return e
}

// CaptureError reports err with optional tags.
func CaptureError(err error, tags map[string]string) {
	if err == nil || !Enabled() {
		return
	}
	sentry.WithScope(func(s *sentry.Scope) {
		s.SetTags(tags)
		sentry.CaptureException(err)
	})
}

// Recoverer reports panics in HTTP handlers and answers 500 instead of
// dropping the connection. It is installed whether or not reporting is on.
func Recoverer(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				rec := recover()
				if rec == nil || rec == http.ErrAbortHandler {
					if rec != nil {
						panic(rec)
					}
					return
				}
				err := fmt.Errorf("http handler panic: %v", rec)
				if Enabled() {
					sentry.WithScope(func(s *sentry.Scope) {
						s.SetTag("route", r.Method+" "+r.Pattern)
						s.SetTag("request_id", w.Header().Get("X-Request-ID"))
						sentry.CurrentHub().Recover(rec)
					})
				}
				if logger != nil {
					// Logged at WARN: the panic itself has just been reported;
					// an ERROR record would report it a second time.
					logger.Warn("recovered handler panic", "error", err, "route", r.Method+" "+r.Pattern)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":"internal error","code":"INTERNAL"}`))
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// tagAttrs are slog attributes promoted to searchable tags.
var tagAttrs = map[string]bool{"job_type": true, "event_type": true, "request_id": true, "worker": true, "service": true, "kind": true}

// SlogHandler wraps next so that ERROR records are also reported. The
// message groups reports (the fingerprint), attributes go in the "log"
// context, and a
// few known keys become tags.
func SlogHandler(next slog.Handler) slog.Handler { return &slogHandler{next: next} }

type slogHandler struct {
	next  slog.Handler
	attrs []slog.Attr
	group string
}

func (h *slogHandler) Enabled(ctx context.Context, l slog.Level) bool { return h.next.Enabled(ctx, l) }

func (h *slogHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError && Enabled() {
		extra := map[string]any{}
		tags := map[string]string{"logger": "slog"}
		add := func(a slog.Attr) {
			key := a.Key
			if h.group != "" {
				key = h.group + "." + key
			}
			v := a.Value.Resolve().String()
			extra[key] = v
			if tagAttrs[a.Key] {
				tags[a.Key] = v
			}
		}
		for _, a := range h.attrs {
			add(a)
		}
		r.Attrs(func(a slog.Attr) bool { add(a); return true })
		msg := r.Message
		if e, ok := extra["error"]; ok {
			msg = fmt.Sprintf("%s: %v", r.Message, e)
		}
		keys := make([]string, 0, len(extra))
		for k := range extra {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		ev := sentry.NewEvent()
		ev.Level = sentry.LevelError
		ev.Message = msg
		extra["attr_keys"] = strings.Join(keys, ",")
		ev.Contexts = map[string]sentry.Context{"log": extra}
		ev.Tags = tags
		ev.Fingerprint = []string{"slog", r.Message}
		sentry.CaptureEvent(ev)
	}
	return h.next.Handle(ctx, r)
}

func (h *slogHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &slogHandler{next: h.next.WithAttrs(as), attrs: append(append([]slog.Attr{}, h.attrs...), as...), group: h.group}
}

func (h *slogHandler) WithGroup(name string) slog.Handler {
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &slogHandler{next: h.next.WithGroup(name), attrs: h.attrs, group: g}
}
