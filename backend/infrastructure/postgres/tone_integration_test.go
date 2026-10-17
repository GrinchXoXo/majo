package postgres

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/tone"
	"github.com/middleman/backend/internal/workflow"
)

// forward is the channel provider: it hands every message to the fake
// messenger, so the test sees exactly what WhatsApp would get.
type forward struct{ m *fakeMessenger }

func (f forward) SendTextWithID(ctx context.Context, to, body string) (string, error) {
	return "wamid." + to, f.m.SendText(ctx, to, body)
}

// friendly is a stand-in model: it only adds an opener, which passes every check.
type friendly struct{ seen []string }

func (f *friendly) RewriteTone(_ context.Context, masked string) (string, string, error) {
	f.seen = append(f.seen, masked)
	return "Hello o! " + masked, "fake-model", nil
}

// Pilot batch item 7: a real buyer notice from the workflow goes through the
// tone layer (on), is stored as sent with its original, and the facts reach
// the buyer unchanged; in shadow mode the buyer gets the template text.
func TestIntegration_ToneLayerOnBuyerNotice(t *testing.T) {
	for _, mode := range []tone.Mode{tone.On, tone.Shadow} {
		t.Run(string(mode), func(t *testing.T) {
			phone := "2348070000091"
			if mode == tone.Shadow {
				phone = "2348070000092"
			}
			db := testDB(t, migrationsPath())
			s := seedTimeout(t, db, phone, "SENT", overdue)
			fake := &fakeMessenger{}
			logger := slog.New(slog.NewTextHandler(io.Discard, nil))
			convs := NewConversationRepository(db)
			model := &friendly{}
			sender := conversation.NewRecordingSender(forward{fake}, convs, "whatsapp", logger)
			sender.SetToner(tone.New(tone.Config{Mode: mode}, model, logger))

			cfg := workflow.Config{Flags: autonomy.AllOn(), Params: autonomy.DefaultParams(), JobMaxAttempts: 2}
			auto, wf, rr := NewAutonomyRepository(db), NewWorkflowRepository(db), NewRoutingRepository(db)
			jobStore := NewJobRepository(db)
			engine := workflow.New(cfg, workflow.Deps{Store: auto, Context: wf, Requests: &fakeRequests{db: db, msgr: fake},
				ReqRepo: NewRequestRepository(db), Router: &fakeRouter{db: db}, Routes: rr, Extractor: noExtractor{}, Messenger: sender,
				Convs: convs, Jobs: jobStore, Events: NewEventWriter(db), Logger: logger,
				Async:          func(f func(context.Context)) { f(context.Background()) },
				VendorRequests: rr, Recovery: wf, Notices: auto, Holds: auto, OfferReverter: NewOfferRepository(db), Pauses: auto})
			engine.Register(jobs.NewWorker(jobStore, jobs.WorkerConfig{PollInterval: 10 * time.Millisecond, Lease: time.Minute, Concurrency: 1}, logger))

			if err := engine.HandleVendorTimeout(context.Background(), s.vr, "deadline"); err != nil {
				t.Fatal(err)
			}
			notes := fake.byKind(workflow.KindVendorTimeoutNotice)
			if len(notes) != 1 {
				t.Fatalf("one notice, got %d", len(notes))
			}
			got := notes[0].body
			var text string
			var metaRaw []byte
			if err := db.QueryRow(`SELECT m.content->>'text', m.metadata FROM messages m JOIN conversations c ON c.id = m.conversation_id
				WHERE c.external_id = $1 AND m.direction = 'OUTBOUND' AND m.metadata->>'kind' = $2`, phone, workflow.KindVendorTimeoutNotice).Scan(&text, &metaRaw); err != nil {
				t.Fatal(err)
			}
			var meta struct {
				Tone map[string]any `json:"tone"`
			}
			_ = json.Unmarshal(metaRaw, &meta)
			if text != got {
				t.Fatalf("stored as sent: %q vs %q", text, got)
			}
			if !strings.Contains(got, "iphone 13") {
				t.Fatalf("the product reaches the buyer: %q", got)
			}
			for _, seen := range model.seen {
				if strings.Contains(seen, "iphone") || strings.ContainsAny(regexp.MustCompile(`\[\[F\d+\]\]`).ReplaceAllString(seen, ""), "0123456789") {
					t.Fatalf("facts reached the model: %q", seen)
				}
			}
			switch mode {
			case tone.On:
				if !strings.HasPrefix(got, "Hello o! ") || meta.Tone["result"] != "applied" || meta.Tone["original"] != strings.TrimPrefix(got, "Hello o! ") {
					t.Fatalf("applied: %q %v", got, meta.Tone)
				}
			case tone.Shadow:
				if strings.HasPrefix(got, "Hello o! ") || meta.Tone["result"] != "shadow" || meta.Tone["candidate"] != "Hello o! "+got {
					t.Fatalf("shadow: template sent, candidate stored: %q %v", got, meta.Tone)
				}
			}
			// A vendor-facing message through the same sender is never rephrased.
			_ = sender.SendText(conversation.WithOutboundPurpose(context.Background(), conversation.OutboundPurpose{Kind: "vendor_request"}), "v"+phone, "Majo request MM-1")
			if vs := fake.byKind("vendor_request"); len(vs) != 1 || vs[0].body != "Majo request MM-1" {
				t.Fatalf("vendor text untouched: %+v", vs)
			}
		})
	}
}
