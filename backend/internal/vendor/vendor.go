// Package vendor owns vendor identity, categories, service areas, and
// basic activity tracking. Per PRD 2 §6: onboarding is operator-driven
// during the pilot — there is no public vendor registration flow.
package vendor

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	StatusPending  Status = "PENDING"
	StatusActive   Status = "ACTIVE"
	StatusInactive Status = "INACTIVE"
	StatusBlocked  Status = "BLOCKED"
)

// Vendor is a person or business that can supply products relevant to
// Majo requests. Only ACTIVE vendors are eligible for matching
// (PRD 2 §5.1).
type Vendor struct {
	ID                     uuid.UUID
	BusinessName           string
	WhatsAppExternalID     string
	Phone                  string
	Status                 Status
	PrimaryLocation        string
	Notes                  string
	Categories             []string
	ServiceAreas           []string
	RequestsReceived       int
	ResponsesReceived      int
	LastResponseAt         *time.Time
	LastMessageAt          *time.Time
	LastSupplySubmissionAt *time.Time
	CreatedAt              time.Time
	UpdatedAt              time.Time
}

// Fields is the correctable/settable vendor field set, mirroring the
// request.Fields pattern used in Phase 1 for consistency.
type Fields struct {
	BusinessName    *string
	Phone           *string
	Status          *Status
	PrimaryLocation *string
	Notes           *string
	Categories      []string // nil = leave unchanged; non-nil = replace entirely
	ServiceAreas    []string
}

// Repository is the persistence boundary for vendors.
type Repository interface {
	Create(ctx context.Context, v *Vendor) (*Vendor, error)
	Update(ctx context.Context, id uuid.UUID, fields Fields) (*Vendor, error)
	FindByID(ctx context.Context, id uuid.UUID) (*Vendor, error)
	FindByWhatsAppID(ctx context.Context, whatsappID string) (*Vendor, error)
	List(ctx context.Context, statusFilter *Status, categoryFilter *string) ([]*Vendor, error)
	// ActiveByCategory returns ACTIVE vendors carrying the given category,
	// the candidate pool for matching (PRD 2 §17).
	ActiveByCategory(ctx context.Context, category string) ([]*Vendor, error)
	RecordMessageActivity(ctx context.Context, vendorID uuid.UUID) error
	RecordSupplySubmission(ctx context.Context, vendorID uuid.UUID) error
	RecordResponseActivity(ctx context.Context, vendorID uuid.UUID) error
	RecordRequestSent(ctx context.Context, vendorID uuid.UUID) error
}
