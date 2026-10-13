package supply

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/audit"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
)

// ListingService turns a vendor-submitted SupplySignal into durable,
// discoverable marketplace supply (PRD 3 §5-6). Publishing is an
// explicit operator action, not automatic — a signal is evidence, not
// yet a marketplace commitment.
type ListingService struct {
	signals  Repository
	listings ListingRepository
	vendors  VendorLookup
	media    SignalMediaReader
	events   event.Writer
	audits   audit.Writer
	logger   *slog.Logger

	// policy is the canonical supply eligibility policy (Phase 4A.5): it sets
	// lifecycle ceilings and answers eligibility. Default: DefaultPolicyConfig.
	policy *EligibilityPolicy

	// Phase 4A.3 automatic DRAFT creation (optional; see SetDraftJobs).
	jobs          jobs.Store
	maxAttempts   int
	draftListener DraftListener
}

func NewListingService(signals Repository, listings ListingRepository, vendors VendorLookup, media SignalMediaReader,
	events event.Writer, audits audit.Writer, logger *slog.Logger) *ListingService {
	return &ListingService{signals: signals, listings: listings, vendors: vendors, media: media,
		events: events, audits: audits, logger: logger, maxAttempts: 1, policy: MustDefaultPolicy()}
}

// SetEligibilityPolicy replaces the default policy (configured values).
func (s *ListingService) SetEligibilityPolicy(p *EligibilityPolicy) {
	if p != nil {
		s.policy = p
	}
}

// Policy returns the canonical eligibility policy.
func (s *ListingService) Policy() *EligibilityPolicy { return s.policy }

// PublishFromSignal is the Phase 3 operator entry point (POST
// /api/supply-listings). Despite its name it never publishes: it builds a
// DRAFT (or returns the existing/refreshed listing) through the canonical
// Phase 4A.3 path, CreateFromSignal. Publication stays a separate operator
// action (Publish, Phase 4A Step 4).
func (s *ListingService) PublishFromSignal(ctx context.Context, signalID uuid.UUID, operatorID *uuid.UUID) (*Listing, error) {
	l, _, err := s.CreateFromSignal(ctx, signalID, event.ActorOperator, operatorID)
	return l, err
}

func notEqualStrPtr(a, b *string) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return *a != *b
}
func notEqualInt64Ptr(a, b *int64) bool {
	if a == nil && b == nil {
		return false
	}
	if a == nil || b == nil {
		return true
	}
	return *a != *b
}
func derefStrPtr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}
func derefInt64Ptr(i *int64) any {
	if i == nil {
		return nil
	}
	return *i
}
