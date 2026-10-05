// Package request owns BuyerRequest: the structured representation of what
// a buyer is looking for. Per PRD 1 §5-6/§48: this is deliberately separate
// from the raw WhatsApp message and from the AI's extraction — "AI
// extracted something != Middleman confirmed the request."
package request

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Status is the request lifecycle. CORRECTED is intentionally not a
// status (per PRD 1 §6) — corrections are tracked via audit/events while
// the status remains NEEDS_REVIEW until the operator confirms.
type Status string

const (
	StatusExtracting  Status = "EXTRACTING"
	StatusNeedsReview Status = "NEEDS_REVIEW"
	StatusConfirmed   Status = "CONFIRMED"
)

// Completeness is an operator-assistance signal, not a gate (PRD 1 §29-30):
// the system must never reject a request merely because optional fields
// are missing.
type Completeness string

const (
	CompletenessComplete   Completeness = "complete"
	CompletenessPartial    Completeness = "partially_complete"
	CompletenessInsufficient Completeness = "insufficient"
)

// BuyerRequest represents the buyer's current procurement need.
type BuyerRequest struct {
	ID              uuid.UUID
	ConversationID  uuid.UUID
	BuyerID         uuid.UUID
	OriginMessageID *uuid.UUID

	Status Status

	Category *string
	Product  *string
	Variant  *string

	BudgetMin *int64
	BudgetMax *int64
	Currency  string

	Location    *string
	Condition   *string
	Quantity    *int
	Urgency     *string
	Preferences []string

	RawMessage    string
	ExtractedData map[string]any // original AI output — never overwritten by corrections
	Confidence    float64

	Completeness Completeness

	CreatedAt   time.Time
	UpdatedAt   time.Time
	ConfirmedAt *time.Time
}

// CalculateCompleteness applies the lightweight rule from PRD 1 §30:
// product identified + budget known + location known are the signals
// used to assist the operator, not to gate anything.
func (r *BuyerRequest) CalculateCompleteness() Completeness {
	hasProduct := r.Product != nil && *r.Product != ""
	hasBudget := r.BudgetMin != nil || r.BudgetMax != nil
	hasLocation := r.Location != nil && *r.Location != ""

	switch {
	case hasProduct && hasBudget && hasLocation:
		return CompletenessComplete
	case hasProduct && (hasBudget || hasLocation):
		return CompletenessPartial
	case hasProduct:
		return CompletenessPartial
	default:
		return CompletenessInsufficient
	}
}

// Fields is the set of correctable structured fields, used by the
// correction/audit path to build previous/new value diffs generically.
type Fields struct {
	Category    *string
	Product     *string
	Variant     *string
	BudgetMin   *int64
	BudgetMax   *int64
	Currency    *string
	Location    *string
	Condition   *string
	Quantity    *int
	Urgency     *string
	Preferences []string
}

// Repository is the persistence boundary for BuyerRequest.
type Repository interface {
	Create(ctx context.Context, r *BuyerRequest) (*BuyerRequest, error)
	FindByID(ctx context.Context, id uuid.UUID) (*BuyerRequest, error)
	List(ctx context.Context, statusFilter *Status) ([]*BuyerRequest, error)
	UpdateExtraction(ctx context.Context, id uuid.UUID, fields Fields, extractedData map[string]any, confidence float64, newStatus Status) (*BuyerRequest, error)
	UpdateFields(ctx context.Context, id uuid.UUID, fields Fields) (*BuyerRequest, error)
	Confirm(ctx context.Context, id uuid.UUID) (*BuyerRequest, error)
	// FindLatestOpenByConversation supports the clarification flow (PRD 1 §28):
	// a follow-up buyer message should continue the same request rather than
	// always starting a new one, if one is still open (not yet confirmed).
	FindLatestOpenByConversation(ctx context.Context, conversationID uuid.UUID) (*BuyerRequest, error)
}
