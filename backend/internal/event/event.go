// Package event defines Middleman's append-only business event model.
// Per PRD 0.2 §15-17: events are persisted directly to PostgreSQL for now.
// No broker (Kafka/NATS/RabbitMQ) is introduced until there is measurable
// evidence it is needed.
package event

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Type is a closed-ish vocabulary of meaningful business events.
// New event types should be added here so the whole system has one
// source of truth for "what can happen."
type Type string

const (
	MessageReceived Type = "MESSAGE_RECEIVED"
	MessageSent     Type = "MESSAGE_SENT"

	RequestCreated   Type = "REQUEST_CREATED"
	RequestExtracted Type = "REQUEST_EXTRACTED"
	RequestCorrected Type = "REQUEST_CORRECTED"

	VendorsSuggested        Type = "VENDORS_SUGGESTED"
	VendorShortlistModified Type = "VENDOR_SHORTLIST_MODIFIED"
	VendorsRouted           Type = "VENDORS_ROUTED"

	VendorResponseReceived Type = "VENDOR_RESPONSE_RECEIVED"

	OfferCreated   Type = "OFFER_CREATED"
	OfferUpdated   Type = "OFFER_UPDATED"
	OfferPresented Type = "OFFER_PRESENTED"

	BuyerSelected Type = "BUYER_SELECTED"
	BuyerPassed   Type = "BUYER_PASSED"

	// RequestConfirmed was not in the original PRD 0 FINAL vocabulary but
	// is introduced in Phase 1 per PRD 1 §22 ("the event vocabulary may
	// evolve when actual product workflows require it").
	RequestConfirmed Type = "REQUEST_CONFIRMED"
	// ClarificationSent is fired when Middleman asks the buyer a
	// clarifying question because the request was too ambiguous (PRD 1 §28).
	ClarificationSent Type = "CLARIFICATION_SENT"

	// --- Phase 2 vocabulary (PRD 2 §48) ---
	VendorCreated     Type = "VENDOR_CREATED"
	VendorUpdated     Type = "VENDOR_UPDATED"
	VendorActivated   Type = "VENDOR_ACTIVATED"
	VendorDeactivated Type = "VENDOR_DEACTIVATED"

	SupplySignalCreated Type = "SUPPLY_SIGNAL_CREATED"
	SupplySignalUpdated Type = "SUPPLY_SIGNAL_UPDATED"
	SupplySignalExpired Type = "SUPPLY_SIGNAL_EXPIRED"

	VendorMatched Type = "VENDOR_MATCHED"

	// --- Phase 2.1 vocabulary ---
	VendorResponseCorrected            Type = "VENDOR_RESPONSE_CORRECTED"
	VendorResponseAssociationCorrected Type = "VENDOR_RESPONSE_ASSOCIATION_CORRECTED"

	// --- Phase 3 vocabulary (supply listings, offers, transactions) ---
	SupplyListingCreated            Type = "SUPPLY_LISTING_CREATED"
	SupplyListingUpdated            Type = "SUPPLY_LISTING_UPDATED"
	SupplyListingExpired            Type = "SUPPLY_LISTING_EXPIRED"
	SupplyListingDuplicateRefreshed Type = "SUPPLY_LISTING_DUPLICATE_REFRESHED"

	OfferReviewed Type = "OFFER_REVIEWED"
	OfferSent     Type = "OFFER_SENT"
	OfferAccepted Type = "OFFER_ACCEPTED"
	OfferRejected Type = "OFFER_REJECTED"
	OfferExpired  Type = "OFFER_EXPIRED"

	BuyerSelectionCreated Type = "BUYER_SELECTION_CREATED"
	TransactionCreated    Type = "TRANSACTION_CREATED"

	// --- Phase 3.1 vocabulary (offer delivery, selection loop, §3A fixes) ---
	// OfferSent (above) = delivered to the buyer's channel. OfferSendFailed
	// = the delivery attempt was rejected; the offer stays deliverable.
	OfferSendFailed Type = "OFFER_SEND_FAILED"
	// OfferWithdrawn: the offer's source response stopped being eligible
	// (e.g. corrected to UNAVAILABLE) before the buyer selected it.
	OfferWithdrawn Type = "OFFER_WITHDRAWN"
	// BuyerReplyUnresolved: a buyer reply in an offer-selection context
	// could not be deterministically mapped to an offer (ambiguous, out of
	// range, or no longer available). No state was changed.
	BuyerReplyUnresolved Type = "BUYER_REPLY_UNRESOLVED"

	VendorRequestSendFailed           Type = "VENDOR_REQUEST_SEND_FAILED"
	VendorResponseAssociationRejected Type = "VENDOR_RESPONSE_ASSOCIATION_REJECTED"
)

// ActorType identifies who/what caused an event.
type ActorType string

const (
	ActorSystem   ActorType = "SYSTEM"
	ActorOperator ActorType = "OPERATOR"
	ActorBuyer    ActorType = "BUYER"
	ActorVendor   ActorType = "VENDOR"
)

// Event is a single append-only record of something meaningful happening.
type Event struct {
	ID         uuid.UUID
	EventType  Type
	EntityType string
	EntityID   *uuid.UUID
	ActorType  ActorType
	ActorID    *uuid.UUID
	Payload    map[string]any
	CreatedAt  time.Time
}

// Writer persists events. Kept as a narrow interface so callers never
// depend on the storage implementation directly.
type Writer interface {
	Write(ctx context.Context, e *Event) error
}

// Reader retrieves events for display (e.g. the dashboard's event history
// view, PRD 0.3 §26).
type Reader interface {
	ListByEntity(ctx context.Context, entityType string, entityID uuid.UUID) ([]*Event, error)
}
