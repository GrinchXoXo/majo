package conversation

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/google/uuid"
)

type bodySender struct{ sent []string }

func (f *bodySender) SendTextWithID(_ context.Context, _, body string) (string, error) {
	f.sent = append(f.sent, body)
	return "wamid.x", nil
}

type upperToner struct {
	calls int
	keep  []string
}

func (u *upperToner) Tone(_ context.Context, text string, keep []string) (string, map[string]any) {
	u.calls++
	u.keep = keep
	return "toned: " + text, map[string]any{"result": "applied", "original": text}
}

// Only buyer-facing text goes through the tone layer, and the message is
// stored as it was sent, with the tone metadata.
func TestRecordingSenderTone(t *testing.T) {
	send, repo, toner := &bodySender{}, &memRepo{convID: uuid.New()}, &upperToner{}
	r := NewRecordingSender(send, repo, "whatsapp", slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx := context.Background()

	_ = r.SendText(WithOutboundPurpose(ctx, OutboundPurpose{Kind: "vendor_request"}), "234", "vendor text")
	if toner.calls != 0 || send.sent[0] != "vendor text" {
		t.Fatal("no toner set yet")
	}
	r.SetToner(toner)
	_ = r.SendText(WithOutboundPurpose(ctx, OutboundPurpose{Kind: "vendor_request"}), "234", "vendor text")
	_ = r.SendText(ctx, "234", "no purpose")
	if toner.calls != 0 {
		t.Fatal("vendor and untagged messages are never toned")
	}
	_ = r.SendText(WithOutboundPurpose(ctx, OutboundPurpose{Kind: "clarification", Audience: AudienceBuyer, Keep: []string{"laptop"}}), "234", "buyer text")
	if toner.calls != 1 || send.sent[3] != "toned: buyer text" || len(toner.keep) != 1 || toner.keep[0] != "laptop" {
		t.Fatalf("buyer text toned: %v %v", send.sent, toner.keep)
	}
	last := repo.saved[len(repo.saved)-1]
	if last.Content["text"] != "toned: buyer text" {
		t.Fatalf("stored as sent: %v", last.Content)
	}
	if tm, ok := last.Metadata["tone"].(map[string]any); !ok || tm["original"] != "buyer text" || last.Metadata["kind"] != "clarification" {
		t.Fatalf("tone metadata stored: %v", last.Metadata)
	}
	if _, ok := repo.saved[0].Metadata["tone"]; ok {
		t.Fatal("no tone metadata on untoned messages")
	}
}
