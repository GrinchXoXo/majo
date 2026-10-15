package postgres

import (
	"context"
	"testing"
	"time"

	"github.com/middleman/backend/internal/conversation"
)

// Pilot batch item 3: the 24-hour window lookup counts only messages the
// contact sent, on the right channel and number.
func TestIntegration_LastInboundAt(t *testing.T) {
	db := testDB(t, migrationsPath())
	ctx := context.Background()
	repo := NewConversationRepository(db)

	if at, err := repo.LastInboundAt(ctx, "whatsapp", "2348000000009"); err != nil || at != nil {
		t.Fatalf("never wrote: nil, got %v %v", at, err)
	}
	conv, err := repo.FindOrCreateByExternalID(ctx, "whatsapp", "2348000000009")
	if err != nil {
		t.Fatal(err)
	}
	in1, _ := repo.SaveMessage(ctx, &conversation.Message{ConversationID: conv.ID, Direction: conversation.DirectionInbound,
		ExternalMessageID: "wamid.in1", MessageType: "text", Content: map[string]any{"text": "I get am"}})
	mustExec(t, db, `UPDATE messages SET created_at = now() - interval '30 hours' WHERE id = $1`, in1.ID)
	_, _ = repo.SaveMessage(ctx, &conversation.Message{ConversationID: conv.ID, Direction: conversation.DirectionOutbound,
		ExternalMessageID: "wamid.out1", MessageType: "text", Content: map[string]any{"text": "Majo Request"}})

	at, err := repo.LastInboundAt(ctx, "whatsapp", "2348000000009")
	if err != nil || at == nil || time.Since(*at) < 29*time.Hour {
		t.Fatalf("only the inbound counts (30h ago), got %v %v", at, err)
	}
	_, _ = repo.SaveMessage(ctx, &conversation.Message{ConversationID: conv.ID, Direction: conversation.DirectionInbound,
		ExternalMessageID: "wamid.in2", MessageType: "text", Content: map[string]any{"text": "hello"}})
	at, _ = repo.LastInboundAt(ctx, "whatsapp", "2348000000009")
	if at == nil || time.Since(*at) > time.Minute {
		t.Fatalf("latest inbound: %v", at)
	}
	if other, _ := repo.LastInboundAt(ctx, "whatsapp", "2348000000010"); other != nil {
		t.Fatalf("another number: nil, got %v", other)
	}
}
