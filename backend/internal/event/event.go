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
	// Phase 4A Step 4 (Architecture Lock V2): the operator publication gate.
	// PUBLISHED = the listing crossed DRAFT -> ACTIVE (emitted once per real
	// transition, never for creation or edits). REJECTED = an operator
	// archived it (DRAFT/ACTIVE -> ARCHIVED, with an optional reason).
	SupplyListingPublished Type = "SUPPLY_LISTING_PUBLISHED"
	SupplyListingRejected  Type = "SUPPLY_LISTING_REJECTED"
	// Phase 4A.5: an explicit verification of a listing's availability
	// (today: operator verification). Moves last_verified_at only.
	SupplyListingVerified Type = "SUPPLY_LISTING_VERIFIED"

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

	VendorRequestSendFailed Type = "VENDOR_REQUEST_SEND_FAILED"

	// --- Phase 3.1 §23A: delivery receipts (from Meta status callbacks) ---
	// Written once per outbound message on its FIRST transition to each
	// state. EntityType/EntityID are the business entity the message was
	// about (vendor_request, buyer_request, conversation), and the payload
	// carries the message "kind" (vendor_request, offer_delivery, ...).
	MessageDelivered Type = "MESSAGE_DELIVERED"
	MessageRead      Type = "MESSAGE_READ" // only observable if the recipient has read receipts on
	MessageFailed    Type = "MESSAGE_FAILED"

	// TransactionCompleted is RESERVED for the future fulfilment phase. No
	// code path completes a transaction yet; until it exists the funnel's
	// "transaction completed" stage is transactions.status = 'COMPLETED'.
	TransactionCompleted              Type = "TRANSACTION_COMPLETED"
	VendorResponseAssociationRejected Type = "VENDOR_RESPONSE_ASSOCIATION_REJECTED"

	// --- Phase 4A.1: durable media ingestion ---
	// MediaAssetStored means Majo has durably stored the asset and can
	// retrieve it (written in the same DB transaction that creates the
	// media_assets row, after storage.Put succeeded). Not "published".
	MediaAssetStored Type = "MEDIA_ASSET_STORED"
	// MediaIngestionFailed: a MEDIA_DOWNLOAD job failed terminally
	// (permanent error, or retries exhausted). No asset exists.
	MediaIngestionFailed Type = "MEDIA_INGESTION_FAILED"

	// --- Phase 4A.2: supply grouping ---
	// VendorMessageUnresolved: a vendor message (photo, video or text) that
	// is neither supply content nor an attached VendorResponse, e.g. a photo
	// sent right after the vendor answered a request. No state changed; the
	// message and any stored media remain for operator review. Mirrors
	// BUYER_REPLY_UNRESOLVED.
	VendorMessageUnresolved Type = "VENDOR_MESSAGE_UNRESOLVED"

	// --- Phase 4B: Discovery & Distribution (Lock §K.1) ---
	// Only real actions produce events: consent changes, batch creation and
	// send, each delivery send/failure/skip, each buyer interaction.
	DiscoveryOptedIn               Type = "DISCOVERY_OPTED_IN"
	DiscoveryOptedOut              Type = "DISCOVERY_OPTED_OUT"
	DiscoveryPreferenceUpdated     Type = "DISCOVERY_PREFERENCE_UPDATED"
	DiscoveryBatchCreated          Type = "DISCOVERY_BATCH_CREATED"
	DiscoveryBatchSent             Type = "DISCOVERY_BATCH_SENT"
	DiscoveryDeliverySent          Type = "DISCOVERY_DELIVERY_SENT"
	DiscoveryDeliveryFailed        Type = "DISCOVERY_DELIVERY_FAILED"
	DiscoveryDeliverySkipped       Type = "DISCOVERY_DELIVERY_SKIPPED"
	DiscoveryInteraction           Type = "DISCOVERY_INTERACTION"
	DiscoveryInteractionUnresolved Type = "DISCOVERY_INTERACTION_UNRESOLVED"
	// ListingEnquiry: "Ask about it" created (or re-found) a discovery-origin BuyerRequest.
	ListingEnquiry Type = "LISTING_ENQUIRY"
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

// Funnel dimension keys (Phase 3.1 §23A). Marketplace events put the
// identifiers and context needed to measure the buyer funnel under these
// exact payload keys, so funnel queries can rely on one vocabulary:
//
//	buyer_id, buyer_request_id, conversation_id, vendor_id,
//	vendor_request_id, vendor_response_id, offer_id, supply_listing_id,
//	transaction_id, category, product, location, channel, outcome
//
// Only values Majo legitimately knows are included (a nil/empty value is
// omitted rather than guessed). The event's own created_at is the timestamp.
// See docs/analytics/funnel.md.

// Merge combines payload fragments into one payload, dropping nil values,
// empty strings and nil pointers, so events never record "unknown" as if it
// were a fact. Later fragments override earlier ones.
func Merge(parts ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, p := range parts {
		for k, v := range p {
			if isEmpty(v) {
				continue
			}
			out[k] = v
		}
	}
	return out
}

func isEmpty(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case *string:
		return x == nil || *x == ""
	case *int64:
		return x == nil
	case *int:
		return x == nil
	case *uuid.UUID:
		return x == nil
	}
	return false
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
