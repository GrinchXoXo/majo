package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/matching"
	"github.com/middleman/backend/internal/workflow"
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

// Pilot batch item 4: one switch pauses every request's automation; resume
// continues them; a request with its own hold stays held.
func TestIntegration_GlobalPauseStopsEverythingAndResumeContinues(t *testing.T) {
	env := newHardeningEnv(t, "2348070000031", "SENT", overdue, nil)
	v2 := addVendor(t, env.db, "After Global Pause", "2348170000031")
	env.router.cands = func() ([]matching.Candidate, error) { return []matching.Candidate{local(v2)}, nil }
	ctx := context.Background()
	auto := NewAutonomyRepository(env.db)

	if _, _, err := env.engine.PauseAll(ctx, "Godyes", " "); !errors.Is(err, workflow.ErrHoldReasonRequired) {
		t.Fatalf("a reason is required, got %v", err)
	}
	if _, created, err := env.engine.PauseAll(ctx, "Godyes", "wrong messages going to vendors"); err != nil || !created {
		t.Fatalf("pause all: %v", err)
	}
	if _, created, _ := env.engine.PauseAll(ctx, "Godyes", "again"); created {
		t.Fatalf("one active pause")
	}
	if !env.engine.Held(ctx, env.s.req) || !env.engine.Held(ctx, uuid.New()) {
		t.Fatalf("every request is held while paused")
	}
	if err := env.engine.HandleVendorTimeout(ctx, env.s.vr, "deadline"); err != nil {
		t.Fatal(err)
	}
	env.drain(t)
	if env.router.routeCall != 0 || len(env.msgr.sent) != 0 {
		t.Fatalf("no automatic step and no message while paused (routes=%d sent=%d)", env.router.routeCall, len(env.msgr.sent))
	}

	// A second request with its own operator hold.
	other := seedTimeout(t, env.db, "2348070000032", "SENT", overdue)
	if _, _, err := env.engine.Pause(ctx, other.req, "Godyes", "buyer dispute"); err != nil {
		t.Fatal(err)
	}

	if ok, err := env.engine.ResumeAll(ctx, "Godyes", "fixed"); err != nil || !ok {
		t.Fatalf("resume all: %v", err)
	}
	if ok, _ := env.engine.ResumeAll(ctx, "Godyes", "twice"); ok {
		t.Fatalf("resuming when not paused reports false")
	}
	env.drain(t)
	if n := env.count(t, `SELECT count(*) FROM vendor_requests WHERE vendor_id = $1 AND buyer_request_id = $2`, v2.ID, env.s.req); n != 1 {
		t.Fatalf("after resume the request continues, got %d", n)
	}
	if !env.engine.Held(ctx, other.req) {
		t.Fatalf("a request's own hold survives the global resume")
	}
	if n := env.count(t, `SELECT count(*) FROM vendor_requests WHERE buyer_request_id = $1 AND vendor_id = $2`, other.req, v2.ID); n != 0 {
		t.Fatalf("the separately held request was not touched")
	}
	if n := env.count(t, `SELECT count(*) FROM events WHERE event_type IN ('AUTOMATION_PAUSED', 'AUTOMATION_RESUMED') AND actor_type = 'OPERATOR'`); n != 2 {
		t.Fatalf("pause and resume audited once each, got %d", n)
	}
	if n := env.count(t, `SELECT count(*) FROM workflow_decisions WHERE workflow_type = 'control' AND reason IN ('paused_all', 'resumed_all')`); n != 2 {
		t.Fatalf("decision log, got %d", n)
	}
	recent, err := auto.RecentPauses(ctx, 10)
	if err != nil || len(recent) != 1 || recent[0].ReleasedAt == nil || *recent[0].ReleasedBy != "Godyes" || recent[0].Reason != "wrong messages going to vendors" {
		t.Fatalf("history: %+v %v", recent, err)
	}
	if p, _ := auto.ActivePause(ctx); p != nil {
		t.Fatalf("no active pause after resume")
	}
}

func TestIntegration_PauseTableAllowsOneActivePause(t *testing.T) {
	db := testDB(t, migrationsPath())
	mustExec(t, db, `INSERT INTO automation_pauses (paused_by, reason) VALUES ('a', 'x')`)
	if _, err := db.Exec(`INSERT INTO automation_pauses (paused_by, reason) VALUES ('b', 'y')`); err == nil {
		t.Fatalf("a second active pause must be refused")
	}
	if _, err := db.Exec(`INSERT INTO automation_pauses (paused_by, reason) VALUES ('b', '  ')`); err == nil {
		t.Fatalf("a blank reason must be refused")
	}
}
