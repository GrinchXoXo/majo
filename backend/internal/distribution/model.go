package distribution

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/discovery"
)

// ---- consent ------------------------------------------------------------

// ConsentStatus is the buyer's discovery consent.
type ConsentStatus string

const (
	OptedIn  ConsentStatus = "OPTED_IN"
	OptedOut ConsentStatus = "OPTED_OUT"
)

// Consent sources.
const (
	SourceButton        = "whatsapp_button"  // tapped the consent prompt / Stop
	SourceKeyword       = "whatsapp_keyword" // typed STOP
	SourceOperatorStaff = "operator_staff"   // explicit staff/test record (INTERNAL only)
)

// Preference is the current discovery consent record of one buyer. It is
// its own record (never a Buyer or BuyerRequest field); every change is
// also appended to the history.
type Preference struct {
	ID              uuid.UUID     `json:"id"`
	BuyerID         uuid.UUID     `json:"buyer_id"`
	Recipient       string        `json:"recipient,omitempty"`
	Status          ConsentStatus `json:"status"`
	Location        *string       `json:"location"`
	Interests       []string      `json:"interests"`
	WordingVersion  string        `json:"wording_version"`
	Source          string        `json:"source"`
	SourceMessageID *uuid.UUID    `json:"source_message_id"`
	Channel         string        `json:"channel"`
	CapturedAt      time.Time     `json:"captured_at"`
	OptedOutAt      *time.Time    `json:"opted_out_at"`
	OptOutReason    *string       `json:"opt_out_reason"`
	UpdatedAt       time.Time     `json:"updated_at"`
}

// PreferenceChange is one explicit consent/preference change.
type PreferenceChange struct {
	BuyerID uuid.UUID
	// Status nil = keep the current status (preference-only update; requires
	// an existing record).
	Status          *ConsentStatus
	Location        *string  // nil = keep
	Interests       []string // nil = keep
	WordingVersion  string
	Source          string
	SourceMessageID *uuid.UUID
	ActorType       string // BUYER | OPERATOR
	ActorID         string
	Reason          string
	At              time.Time
}

// ErrNoPreference: a preference-only update for a buyer with no record.
var ErrNoPreference = errors.New("buyer has no discovery preference record")

// ---- batches & deliveries ----------------------------------------------

// Batch statuses.
const (
	BatchPlanned   = "PLANNED"
	BatchSending   = "SENDING"
	BatchSent      = "SENT"
	BatchPartial   = "PARTIAL"
	BatchFailed    = "FAILED"
	BatchCancelled = "CANCELLED"
)

// Delivery statuses.
const (
	DeliveryPlanned = "PLANNED"
	DeliverySending = "SENDING"
	DeliverySent    = "SENT"
	DeliveryFailed  = "FAILED"
	DeliverySkipped = "SKIPPED"
)

// Batch is one operator-triggered discovery run.
type Batch struct {
	ID                  uuid.UUID       `json:"id"`
	ClientKey           string          `json:"client_key"`
	Status              string          `json:"status"`
	Mode                Mode            `json:"mode"`
	RankingVersion      string          `json:"ranking_version"`
	PresentationVersion string          `json:"presentation_version"`
	Arms                []string        `json:"arms"`
	Scope               discovery.Scope `json:"scope"`
	Plan                PlanReport      `json:"plan"`
	CreatedBy           string          `json:"created_by,omitempty"`
	Note                string          `json:"note,omitempty"`
	CreatedAt           time.Time       `json:"created_at"`
	UpdatedAt           time.Time       `json:"updated_at"`
	SendStartedAt       *time.Time      `json:"send_started_at"`
	SentAt              *time.Time      `json:"sent_at"`
}

// EligibilitySnapshot is the canonical policy result recorded with a
// planned candidate (for diagnostics: "why was this shown to this buyer").
type EligibilitySnapshot struct {
	Eligible       bool     `json:"eligible"`
	LifecycleDay   int      `json:"lifecycle_day"`
	LifecycleMax   int      `json:"lifecycle_max_days"`
	ExpiresAt      string   `json:"expires_at"`
	Freshness      string   `json:"freshness"`
	VerifiedAt     *string  `json:"verified_at"`
	GeoStage       string   `json:"geo_stage"`
	GeoReason      string   `json:"geo_reason"`
	ViewerLevel    string   `json:"viewer_level"`
	ViewerEligible bool     `json:"eligible_for_viewer"`
	ViewerReason   string   `json:"viewer_reason"`
	Notes          []string `json:"notes"`
}

// PlannedCandidate is a listing planned for a buyer, with its score,
// reasons and the eligibility it had when planned.
type PlannedCandidate struct {
	ListingID           uuid.UUID           `json:"listing_id"`
	PublicReference     string              `json:"public_reference"`
	Product             string              `json:"product"`
	Category            string              `json:"category"`
	Price               *int64              `json:"price"`
	Score               int                 `json:"score"`
	Reasons             []discovery.Reason  `json:"reasons"`
	Eligibility         EligibilitySnapshot `json:"eligibility"`
	Images              int                 `json:"images"`
	HasVideo            bool                `json:"has_video"`
	NeedsReconfirmation bool                `json:"needs_reconfirmation"`
}

// Delivery is what one buyer was (to be) shown in one batch.
type Delivery struct {
	ID                 uuid.UUID          `json:"id"`
	BatchID            uuid.UUID          `json:"batch_id"`
	BuyerID            uuid.UUID          `json:"buyer_id"`
	Recipient          string             `json:"recipient"`
	Arm                string             `json:"arm"`
	Status             string             `json:"status"`
	DedupeKey          string             `json:"dedupe_key"`
	ConsentStatus      string             `json:"consent_status"`
	PreferenceID       *uuid.UUID         `json:"preference_id"`
	ListingIDs         []uuid.UUID        `json:"listing_ids"`
	Candidates         []PlannedCandidate `json:"candidates"`
	Presentation       string             `json:"presentation,omitempty"`
	SkipReason         string             `json:"skip_reason,omitempty"`
	Error              string             `json:"error,omitempty"`
	MessageID          *uuid.UUID         `json:"message_id"`
	ProviderMessageID  string             `json:"provider_message_id,omitempty"`
	PlannedAt          time.Time          `json:"planned_at"`
	ClaimedAt          *time.Time         `json:"claimed_at"`
	SentAt             *time.Time         `json:"sent_at"`
	FirstInteractionAt *time.Time         `json:"first_interaction_at"`
	LastInteractionAt  *time.Time         `json:"last_interaction_at"`
	InteractionCount   int                `json:"interaction_count"`
	LastAction         string             `json:"last_action,omitempty"`
}

// DeliveryDedupeKey is stable and deterministic: one delivery per batch and buyer.
func DeliveryDedupeKey(batchID, buyerID uuid.UUID) string {
	return "batch:" + batchID.String() + ":buyer:" + buyerID.String()
}

// ---- audience -----------------------------------------------------------

// RecentRequest is a buyer request summary used for the viewer context.
type RecentRequest struct {
	Category  *string
	Product   *string
	BudgetMax *int64
	Location  *string
	CreatedAt time.Time
}

// AudienceMember is everything Distribution knows about one potential
// recipient when planning or sending.
type AudienceMember struct {
	BuyerID        uuid.UUID
	ConversationID *uuid.UUID
	Recipient      string
	Preference     *Preference
	RecentRequests []RecentRequest // newest first
	// NegativeCategories from "not interested" within the negative window.
	NegativeCategories []string
	// Exposed: listings delivered to this buyer within the exposure window.
	Exposed map[uuid.UUID]time.Time
	// DeliveriesInCapPeriod: SENDING/SENT deliveries within the cap period.
	DeliveriesInCapPeriod int
	// LastInboundAt: the buyer's last inbound message (24h session window).
	LastInboundAt *time.Time
}

// AudienceQuery selects audience members.
type AudienceQuery struct {
	// Recipients restricts the audience to these channel addresses; empty =
	// every buyer with a discovery preference record.
	Recipients    []string
	ExposureSince time.Time
	CapSince      time.Time
	NegativeSince time.Time
}

// ClaimRules are re-checked atomically when a delivery is claimed for sending.
type ClaimRules struct {
	CapSince      time.Time
	MaxPerBuyer   int
	ExposureSince time.Time
	// Keep is the (re-evaluated, still eligible) listing set to present, in order.
	Keep []uuid.UUID
}

// ClaimResult is the outcome of claiming a delivery.
type ClaimResult struct {
	Claimed    bool
	SkipReason string
	Listings   []uuid.UUID // what will be presented (after dedupe)
	Deduped    []uuid.UUID
}

// Skip / exclusion reasons.
const (
	ReasonNoConsent          = "no_consent"
	ReasonOptedOut           = "opted_out"
	ReasonUnknownBuyer       = "unknown_buyer"
	ReasonFrequencyCap       = "frequency_cap"
	ReasonDedupe             = "dedupe"
	ReasonNoCandidates       = "no_suitable_candidates"
	ReasonNoLongerEligible   = "no_longer_eligible"
	ReasonTemplateRequired   = "outside_session_no_template"
	ReasonNotPlanned         = "not_planned"
	ReasonOutsideWindow      = "outside_delivery_window"
	ReasonDeliveryNotPending = "delivery_not_pending"
)

// ---- interactions -------------------------------------------------------

// Interaction outcomes.
const (
	OutcomeApplied    = "APPLIED"
	OutcomeUnresolved = "UNRESOLVED"
)

// Interaction is one structured tap (or STOP keyword), resolved or not.
type Interaction struct {
	ID               uuid.UUID  `json:"id"`
	InboundMessageID uuid.UUID  `json:"inbound_message_id"`
	BuyerID          uuid.UUID  `json:"buyer_id"`
	DeliveryID       *uuid.UUID `json:"delivery_id"`
	RawID            string     `json:"raw_id"`
	Action           string     `json:"action,omitempty"`
	ListingRef       string     `json:"listing_ref,omitempty"`
	ListingID        *uuid.UUID `json:"listing_id"`
	Category         string     `json:"category,omitempty"`
	Outcome          string     `json:"outcome"`
	Reason           string     `json:"reason,omitempty"`
	BuyerRequestID   *uuid.UUID `json:"buyer_request_id"`
	CreatedAt        time.Time  `json:"created_at"`
}

// ---- persistence ----------------------------------------------------------

// Store is Distribution's persistence boundary (PostgreSQL in production).
type Store interface {
	// Consent.
	Preference(ctx context.Context, buyerID uuid.UUID) (*Preference, error)
	// ApplyPreference upserts the preference, appends history and writes the
	// event in ONE transaction. changed=false when nothing changed.
	ApplyPreference(ctx context.Context, c PreferenceChange) (p *Preference, changed bool, err error)
	ListPreferences(ctx context.Context) ([]*Preference, error)
	BuyerIDByRecipient(ctx context.Context, channel, recipient string) (*uuid.UUID, error)

	// Audience.
	Audience(ctx context.Context, q AudienceQuery) (members []AudienceMember, unknownRecipients []string, err error)
	AudienceMember(ctx context.Context, buyerID uuid.UUID, q AudienceQuery) (*AudienceMember, error)
	// HasConsentPrompt: a consent prompt was sent to this buyer since t.
	HasConsentPrompt(ctx context.Context, buyerID uuid.UUID, since time.Time) (bool, error)

	// Supply for re-evaluation at send/interaction time.
	SupplyByIDs(ctx context.Context, ids []uuid.UUID) ([]discovery.SupplyItem, error)

	// Batches and deliveries.
	CreateBatch(ctx context.Context, b *Batch, ds []*Delivery) (*Batch, bool, error)
	FindBatch(ctx context.Context, id uuid.UUID) (*Batch, error)
	ListBatches(ctx context.Context, limit int) ([]*Batch, error)
	ListDeliveries(ctx context.Context, batchID uuid.UUID) ([]*Delivery, error)
	FindDelivery(ctx context.Context, id uuid.UUID) (*Delivery, error)
	// BeginBatchSend moves PLANNED -> SENDING; false if the batch is not PLANNED.
	BeginBatchSend(ctx context.Context, id uuid.UUID, at time.Time) (bool, error)
	FinishBatch(ctx context.Context, id uuid.UUID, status string, at time.Time) error
	// ClaimDelivery atomically re-checks consent, the frequency cap and
	// per-listing exposure for a PLANNED delivery (serialised per buyer),
	// records exposure and moves it to SENDING — or marks it SKIPPED.
	ClaimDelivery(ctx context.Context, deliveryID uuid.UUID, rules ClaimRules, at time.Time) (ClaimResult, error)
	SkipDelivery(ctx context.Context, deliveryID uuid.UUID, reason string) error
	MarkDeliverySent(ctx context.Context, deliveryID uuid.UUID, presentation string, messageID *uuid.UUID, providerMessageID string, at time.Time) error
	// MarkDeliveryFailed records the error and removes the delivery's
	// exposure (nothing reached the buyer).
	MarkDeliveryFailed(ctx context.Context, deliveryID uuid.UUID, presentation, errText string) error

	// Interactions. RecordInteraction is idempotent per inbound message
	// (created=false on a repeat) and bumps the delivery's counters when
	// APPLIED, in one transaction.
	RecordInteraction(ctx context.Context, in *Interaction) (bool, error)
	SetInteractionRequest(ctx context.Context, interactionID, buyerRequestID uuid.UUID) error
	ListInteractions(ctx context.Context, deliveryID uuid.UUID) ([]*Interaction, error)

	// Attribution and funnel (read models over existing entities).
	Attribution(ctx context.Context, deliveryID uuid.UUID) (*Attribution, error)
	Funnel(ctx context.Context, batchID *uuid.UUID) (*Funnel, error)
}

// Attribution answers "what did this delivery lead to?".
type Attribution struct {
	DeliveryID   uuid.UUID               `json:"delivery_id"`
	BatchID      uuid.UUID               `json:"batch_id"`
	Arm          string                  `json:"arm"`
	Messages     []AttributedMessage     `json:"messages"`
	Interactions []*Interaction          `json:"interactions"`
	Requests     []AttributedRequest     `json:"requests"`
	Offers       []AttributedOffer       `json:"offers"`
	Selections   []AttributedSelection   `json:"selections"`
	Transactions []AttributedTransaction `json:"transactions"`
}

type AttributedMessage struct {
	MessageID         uuid.UUID  `json:"message_id"`
	ProviderMessageID string     `json:"provider_message_id"`
	Kind              string     `json:"kind"`
	SentAt            time.Time  `json:"sent_at"`
	DeliveredAt       *time.Time `json:"delivered_at"`
	ReadAt            *time.Time `json:"read_at"`
	FailedAt          *time.Time `json:"failed_at"`
}

type AttributedRequest struct {
	ID        uuid.UUID  `json:"id"`
	Status    string     `json:"status"`
	Origin    string     `json:"origin"`
	ListingID *uuid.UUID `json:"listing_id"`
	CreatedAt time.Time  `json:"created_at"`
}

type AttributedOffer struct {
	ID              uuid.UUID  `json:"id"`
	BuyerRequestID  uuid.UUID  `json:"buyer_request_id"`
	SupplyListingID *uuid.UUID `json:"supply_listing_id"`
	VendorRequestID *uuid.UUID `json:"vendor_request_id"`
	Status          string     `json:"status"`
	Price           *int64     `json:"price"`
}

type AttributedSelection struct {
	ID      uuid.UUID `json:"id"`
	OfferID uuid.UUID `json:"offer_id"`
}

type AttributedTransaction struct {
	ID              uuid.UUID  `json:"id"`
	OfferID         uuid.UUID  `json:"offer_id"`
	SupplyListingID *uuid.UUID `json:"supply_listing_id"`
	Status          string     `json:"status"`
	PaymentStatus   string     `json:"payment_status"`
	GrossAmount     int64      `json:"gross_amount"`
}

// Funnel is the discovery funnel (Lock §K.1), counted from system-of-record
// tables. Exclusions come from batch plans.
type Funnel struct {
	Batches            int            `json:"batches"`
	OptedIn            int            `json:"opted_in"`
	OptedOut           int            `json:"opted_out"`
	EligibleAudience   int            `json:"eligible_audience"`
	EligibleListings   int            `json:"eligible_listings"`
	DeliveriesPlanned  int            `json:"deliveries_planned"`
	DeliveriesAttempt  int            `json:"deliveries_attempted"`
	DeliveriesSent     int            `json:"deliveries_sent"`
	DeliveriesFailed   int            `json:"deliveries_failed"`
	DeliveriesSkipped  int            `json:"deliveries_skipped"`
	Delivered          int            `json:"delivered"`
	Read               int            `json:"read"`
	Interacted         int            `json:"interacted"`
	ListingViewed      int            `json:"listing_viewed"`
	Asked              int            `json:"asked"` // distinct (delivery, listing) "Ask about it"
	RequestsCreated    int            `json:"requests_created"`
	RequestsConfirmed  int            `json:"requests_confirmed"`
	Offers             int            `json:"offers"`
	Selections         int            `json:"selections"`
	Transactions       int            `json:"transactions"`
	StopsFromDiscovery int            `json:"opt_outs_from_discovery"`
	Unresolved         int            `json:"unresolved_interactions"`
	Exclusions         map[string]int `json:"exclusions"`
	ByArm              map[string]int `json:"sent_by_arm"`
}
