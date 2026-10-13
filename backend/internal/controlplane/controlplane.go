// Package controlplane holds the read models of the operator control plane's
// timeline and job views (reuse-audit R1), shared by the HTTP layer and the
// Postgres repository.
package controlplane

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// EventFilter narrows the event timeline. Empty fields match everything.
type EventFilter struct {
	EventType  string
	EntityType string
	EntityID   *uuid.UUID
	ActorType  string
}

// EventRow is one row of the event timeline.
type EventRow struct {
	ID         uuid.UUID       `json:"id"`
	EventType  string          `json:"event_type"`
	EntityType string          `json:"entity_type"`
	EntityID   *uuid.UUID      `json:"entity_id"`
	ActorType  string          `json:"actor_type"`
	ActorID    *uuid.UUID      `json:"actor_id"`
	ActorName  *string         `json:"actor_name"` // named operator, when the actor is one
	Payload    json.RawMessage `json:"payload"`
	CreatedAt  time.Time       `json:"created_at"`
}

// JobFilter narrows the job list.
type JobFilter struct {
	Status  string
	JobType string
}

// JobRow is one durable job, without its payload.
type JobRow struct {
	ID             uuid.UUID  `json:"id"`
	JobType        string     `json:"job_type"`
	IdempotencyKey string     `json:"idempotency_key"`
	Status         string     `json:"status"`
	Attempts       int        `json:"attempts"`
	MaxAttempts    int        `json:"max_attempts"`
	RunAt          time.Time  `json:"run_at"`
	LockedBy       *string    `json:"locked_by"`
	LastError      *string    `json:"last_error"`
	CreatedAt      time.Time  `json:"created_at"`
	UpdatedAt      time.Time  `json:"updated_at"`
	CompletedAt    *time.Time `json:"completed_at"`
}

// JobSummaryRow counts jobs of one type by status.
type JobSummaryRow struct {
	JobType   string `json:"job_type"`
	Pending   int    `json:"pending"`
	Running   int    `json:"running"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
	// OverdueSeconds is how long the oldest due PENDING job has waited past
	// its run_at (0 when nothing is overdue): a stuck worker shows up here.
	OverdueSeconds int64 `json:"overdue_seconds"`
}
