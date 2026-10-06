package postgres

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
)

// A terminal job failure and its failure event are one transaction: both
// are stored, or (lease lost / event write fails) neither is.
func TestIntegration_JobFailAndFailureEventAreAtomic(t *testing.T) {
	db := testDB(t, migrationsPath())
	repo := NewJobRepository(db)
	ctx := context.Background()
	count := func(q string, args ...any) int {
		var n int
		if err := db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	claim := func(key string) *jobs.Job {
		if _, err := repo.Enqueue(ctx, &jobs.Job{Type: "T", IdempotencyKey: key, MaxAttempts: 1, Payload: map[string]any{}}); err != nil {
			t.Fatal(err)
		}
		j, err := repo.Claim(ctx, []string{"T"}, "w1", time.Minute)
		if err != nil || j == nil {
			t.Fatalf("claim: %v", err)
		}
		return j
	}
	entity := uuid.New()

	// 1. Both stored.
	j := claim("a")
	if err := repo.Fail(ctx, j.ID, "w1", "boom", &event.Event{EventType: "TEST_FAILED", EntityType: "message", EntityID: &entity,
		ActorType: event.ActorSystem, Payload: map[string]any{"job_id": j.ID.String()}}); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM jobs WHERE id = $1 AND status = 'FAILED'`, j.ID) != 1 ||
		count(`SELECT count(*) FROM events WHERE event_type = 'TEST_FAILED' AND payload->>'job_id' = $1`, j.ID.String()) != 1 {
		t.Fatal("want FAILED job and its event")
	}

	// 2. Event cannot be written -> the job is not marked FAILED either.
	j = claim("b")
	err := repo.Fail(ctx, j.ID, "w1", "boom", &event.Event{EventType: "TEST_FAILED", EntityType: "message",
		ActorType: event.ActorSystem, Payload: map[string]any{"unencodable": make(chan int)}})
	if err == nil {
		t.Fatal("expected the event write to fail")
	}
	if count(`SELECT count(*) FROM jobs WHERE id = $1 AND status = 'RUNNING'`, j.ID) != 1 {
		t.Fatal("a failed event write must roll back the FAILED transition (the job stays reclaimable)")
	}

	// 3. Lease lost -> neither.
	j = claim("c")
	err = repo.Fail(ctx, j.ID, "other-worker", "boom", &event.Event{EventType: "TEST_FAILED_C", EntityType: "message",
		ActorType: event.ActorSystem, Payload: map[string]any{}})
	if !errors.Is(err, jobs.ErrLeaseLost) || count(`SELECT count(*) FROM events WHERE event_type = 'TEST_FAILED_C'`) != 0 {
		t.Fatalf("lease lost: want ErrLeaseLost and no event, got %v", err)
	}
}
