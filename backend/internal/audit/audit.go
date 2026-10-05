// Package audit records operator/system actions with before/after values.
// This matters especially in Phase 1+, where every AI extraction and vendor
// shortlist is reviewed and possibly corrected by a human operator (per
// Middleman_Technical_Spec_v1 §3): those corrections must be reconstructable
// later even though they are not yet used for ML training.
package audit

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Entry is a single audit record.
type Entry struct {
	ID             uuid.UUID
	ActorType      string // OPERATOR | SYSTEM
	ActorID        *uuid.UUID
	Action         string // e.g. REQUEST_CORRECTED, VENDOR_SHORTLIST_MODIFIED
	EntityType     string
	EntityID       *uuid.UUID
	PreviousValue  map[string]any
	NewValue       map[string]any
	Metadata       map[string]any
	CreatedAt      time.Time
}

// Writer persists audit entries.
type Writer interface {
	Write(ctx context.Context, e *Entry) error
}
