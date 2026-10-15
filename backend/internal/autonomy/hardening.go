package autonomy

// Phase 4C hardening: vendor-request timeout recovery, buyer notices that
// could not be delivered, and operator holds on one workflow.

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Decision kinds added by the hardening pass.
const (
	DecVendorTimeout  = "VENDOR_REQUEST_TIMEOUT" // a vendor request reached its deadline (or could not be sent)
	DecRecoverRequest = "RECOVER_REQUEST"        // what Majo did for a request whose vendors all went quiet / declined
	DecNotifyBuyer    = "NOTIFY_BUYER"           // a buyer-facing message that failed, was queued, or was sent late
	DecHold           = "WORKFLOW_HOLD"          // operator pause / resume of one request's automation
	DecAcknowledge    = "ACKNOWLEDGE_MESSAGE"    // a greeting / courtesy / bare reference handled without a request
	DecRestrictedItem = "RESTRICTED_ITEM"        // request held for review under the restricted-items policy
)

// Exception reasons added by the hardening pass.
const (
	ReasonInvalidRecipient      = "invalid_recipient"           // the stored number cannot receive WhatsApp messages
	ReasonInvalidOutbound       = "invalid_outbound_message"    // Majo built a message the provider rejected as malformed
	ReasonProviderAuthConfig    = "provider_auth_or_config"     // token / permission / registration / billing / template config
	ReasonProviderAccount       = "provider_account_restricted" // account-level integrity or quality enforcement
	ReasonUnclassifiedProvider  = "unclassified_provider_error" // a provider error Majo cannot classify safely
	ReasonRecoveryFailed        = "request_recovery_failed"     // re-routing after a vendor timeout kept failing
	ReasonRestrictedItem        = "restricted_item_review"      // demand matched the restricted-items list
	ReasonBuyerNoticeUndelivery = "buyer_notice_undeliverable"  // a buyer update cannot be delivered by any compliant route
)

// Recovery outcomes (decision reasons) for DecRecoverRequest.
const (
	RecoverRerouted         = "rerouted_to_alternative_vendors"
	RecoverBroadenAsked     = "asked_buyer_to_broaden_area"
	RecoverBuyerTold        = "buyer_told_no_reply_yet"
	RecoverMaxRounds        = "max_recovery_rounds_reached"
	RecoverContactLimit     = "vendor_contact_limit_reached"
	RecoverNotActive        = "buyer_request_not_active"
	RecoverFulfilled        = "already_fulfilled"
	RecoverHasOptions       = "buyer_has_options"
	RecoverAwaiting         = "other_vendors_still_awaited"
	RecoverSuperseded       = "superseded_by_newer_request"
	RecoverHeld             = "workflow_held"
	RecoverDuplicate        = "duplicate_recovery_prevented"
	RecoverDisabled         = "auto_recovery_disabled"
	RecoverStepFailed       = "recovery_step_failed"
	TimeoutExpired          = "expired_at_deadline"
	TimeoutAlreadyResponded = "already_responded"
	TimeoutCancelled        = "already_cancelled"
	TimeoutNeverSent        = "never_sent"
	TimeoutNotDue           = "not_due_rescheduled"
)

// Notice statuses.
const (
	NoticePending   = "PENDING"
	NoticeSent      = "SENT"
	NoticeFailed    = "FAILED"
	NoticeCancelled = "CANCELLED"
)

// NoticeKindOffers is a deferred offer delivery (the offer loop rebuilds and
// delivers whatever is deliverable then; no text is stored).
const NoticeKindOffers = "offer_delivery"

// Notice is a buyer-facing message waiting for a compliant delivery route.
type Notice struct {
	ID              uuid.UUID  `json:"id"`
	ConversationID  uuid.UUID  `json:"conversation_id"`
	BuyerRequestID  *uuid.UUID `json:"buyer_request_id"`
	Kind            string     `json:"kind"`
	Body            string     `json:"body"`
	Status          string     `json:"status"`
	FailureClass    string     `json:"failure_class"`
	FailureCode     int        `json:"failure_code"`
	Attempts        int        `json:"attempts"`
	LastError       string     `json:"last_error,omitempty"`
	SourceMessageID *uuid.UUID `json:"source_message_id"`
	CreatedAt       time.Time  `json:"created_at"`
	SentAt          *time.Time `json:"sent_at"`
}

// Hold is an operator pause of one request's automation.
type Hold struct {
	ID             uuid.UUID  `json:"id"`
	BuyerRequestID uuid.UUID  `json:"buyer_request_id"`
	HeldBy         string     `json:"held_by"`
	Reason         string     `json:"reason"`
	HeldAt         time.Time  `json:"held_at"`
	ReleasedBy     string     `json:"released_by,omitempty"`
	ReleaseReason  string     `json:"release_reason,omitempty"`
	ReleasedAt     *time.Time `json:"released_at"`
}

// NoticeStore persists queued buyer notices.
type NoticeStore interface {
	// QueueNotice stores a PENDING notice. created=false when an equivalent
	// one already exists (same failed source message, or a pending offer
	// re-delivery for the request); the existing one is returned.
	QueueNotice(ctx context.Context, n *Notice) (*Notice, bool, error)
	PendingNotices(ctx context.Context, conversationID uuid.UUID) ([]*Notice, error)
	FindNotice(ctx context.Context, id uuid.UUID) (*Notice, error)
	// MarkNotice moves a PENDING notice to status (SENT, FAILED, CANCELLED)
	// or records a failed attempt (status PENDING). Returns false when it was
	// no longer PENDING (another worker finished it).
	MarkNotice(ctx context.Context, id uuid.UUID, status string, lastErr string) (bool, error)
	NoticesForRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*Notice, error)
}

// HoldStore persists operator holds.
type HoldStore interface {
	// PlaceHold creates the active hold; created=false if one is active.
	PlaceHold(ctx context.Context, buyerRequestID uuid.UUID, by, reason string) (*Hold, bool, error)
	// ReleaseHold ends the active hold; false if none was active.
	ReleaseHold(ctx context.Context, buyerRequestID uuid.UUID, by, reason string) (bool, error)
	ActiveHold(ctx context.Context, buyerRequestID uuid.UUID) (*Hold, error)
	Holds(ctx context.Context, buyerRequestID uuid.UUID) ([]*Hold, error)
}

// ---- global automation pause (pilot batch item 4) ------------------------------

// AutomationPause is an operator's pause of ALL automation (every request is
// treated as held until it is released).
type AutomationPause struct {
	ID            uuid.UUID  `json:"id"`
	PausedBy      string     `json:"paused_by"`
	Reason        string     `json:"reason"`
	PausedAt      time.Time  `json:"paused_at"`
	ReleasedBy    *string    `json:"released_by,omitempty"`
	ReleaseReason *string    `json:"release_reason,omitempty"`
	ReleasedAt    *time.Time `json:"released_at,omitempty"`
}

// PauseStore persists the global pause.
type PauseStore interface {
	// ActivePause returns the active pause, or nil.
	ActivePause(ctx context.Context) (*AutomationPause, error)
	// PlacePause pauses; created=false when already paused (idempotent).
	PlacePause(ctx context.Context, by, reason string) (*AutomationPause, bool, error)
	// ReleasePause releases; false when nothing was paused.
	ReleasePause(ctx context.Context, by, reason string) (bool, error)
	// RecentPauses lists pauses newest first (control-plane history).
	RecentPauses(ctx context.Context, limit int) ([]*AutomationPause, error)
	// OpenConfirmedRequests: confirmed requests created since `since` with
	// no active selection (re-checked on resume).
	OpenConfirmedRequests(ctx context.Context, since time.Time) ([]uuid.UUID, error)
}
