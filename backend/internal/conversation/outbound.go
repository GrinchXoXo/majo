package conversation

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/channel"
)

// OutboundPurpose describes WHY Majo sent a message, so delivery receipts
// that arrive later (delivered / read / failed) can be attributed to the
// business entity and funnel stage they belong to (Phase 3.1 §23A).
type OutboundPurpose struct {
	Kind       string     // e.g. "vendor_request", "offer_delivery", "selection_confirmation", "acknowledgement"
	EntityType string     // e.g. "vendor_request", "buyer_request", "conversation"
	EntityID   *uuid.UUID //
	Extra      map[string]any
}

type outboundPurposeKey struct{}

// WithOutboundPurpose tags the next outbound send(s) made with ctx. Callers
// set it immediately before SendText; the messaging interfaces used by the
// domain services stay exactly as they were.
func WithOutboundPurpose(ctx context.Context, p OutboundPurpose) context.Context {
	return context.WithValue(ctx, outboundPurposeKey{}, p)
}

// OutboundPurposeFrom returns the purpose tagged on ctx, if any.
func OutboundPurposeFrom(ctx context.Context) (OutboundPurpose, bool) {
	p, ok := ctx.Value(outboundPurposeKey{}).(OutboundPurpose)
	return p, ok
}

// IDSender is a channel provider that reports the provider message ID of
// each sent message (whatsapp.CloudAPIProvider implements it).
type IDSender interface {
	SendTextWithID(ctx context.Context, to, body string) (string, error)
}

// RecordingSender wraps a channel provider: it sends exactly as before and,
// on success, persists the OUTBOUND message with the provider message ID and
// its OutboundPurpose. Before §23A outbound messages were not stored at all,
// so whether a vendor request or offer was ever delivered or read could
// never be measured.
//
// Recording is best-effort: a persistence failure is logged and never turns
// a successful send into an error (the message has already been sent).
type RecordingSender struct {
	sender  IDSender
	repo    Repository
	channel string
	logger  *slog.Logger
}

func NewRecordingSender(sender IDSender, repo Repository, channel string, logger *slog.Logger) *RecordingSender {
	return &RecordingSender{sender: sender, repo: repo, channel: channel, logger: logger}
}

// SendText satisfies the local MessagingProvider interfaces in request/,
// routing/ and marketplace/.
func (r *RecordingSender) SendText(ctx context.Context, to, body string) error {
	externalID, err := r.sender.SendTextWithID(ctx, to, body)
	if err != nil {
		return err
	}
	r.record(ctx, to, body, externalID)
	return nil
}

// ErrRichUnsupported: the wrapped provider cannot send rich messages.
var ErrRichUnsupported = errors.New("channel provider does not support rich messages")

// SendRecorded sends a rendered channel message (text, image, video,
// buttons, list, template — Phase 4B) through the provider and persists it
// like SendText does, returning the persisted message ID and the provider
// message ID. Every discovery message goes through here (Lock §G).
func (r *RecordingSender) SendRecorded(ctx context.Context, to string, m channel.Message) (*uuid.UUID, string, error) {
	rich, ok := r.sender.(channel.Sender)
	if !ok {
		return nil, "", ErrRichUnsupported
	}
	externalID, err := rich.SendMessage(ctx, to, m)
	if err != nil {
		return nil, "", err
	}
	saved := r.save(ctx, to, string(m.Kind), map[string]any{"text": m.Summary(), "rich": m}, externalID)
	if saved == nil {
		return nil, externalID, nil
	}
	return &saved.ID, externalID, nil
}

func (r *RecordingSender) record(ctx context.Context, to, body, externalID string) {
	r.save(ctx, to, "text", map[string]any{"text": body}, externalID)
}

func (r *RecordingSender) save(ctx context.Context, to, messageType string, content map[string]any, externalID string) *Message {
	conv, err := r.repo.FindOrCreateByExternalID(ctx, r.channel, to)
	if err != nil || conv == nil {
		r.logger.Error("recording outbound message: resolving conversation", "error", err)
		return nil
	}
	meta := map[string]any{}
	if p, ok := OutboundPurposeFrom(ctx); ok {
		meta["kind"] = p.Kind
		meta["entity_type"] = p.EntityType
		if p.EntityID != nil {
			meta["entity_id"] = p.EntityID.String()
		}
		for k, v := range p.Extra {
			meta[k] = v
		}
	}
	saved, err := r.repo.SaveMessage(ctx, &Message{
		ConversationID:    conv.ID,
		Direction:         DirectionOutbound,
		ExternalMessageID: externalID,
		MessageType:       messageType,
		Content:           content,
		Metadata:          meta,
	})
	if err != nil {
		r.logger.Error("recording outbound message", "external_message_id", externalID, "error", err)
		return nil
	}
	return saved
}

// Delivery statuses from the channel provider (Meta: sent/delivered/read/failed).
const (
	DeliveryDelivered = "delivered"
	DeliveryRead      = "read"
	DeliveryFailed    = "failed"
)

// DeliveryTransition is the result of applying a delivery-status callback.
// The First* flags are true only the first time the message reached that
// state, so receipts (which providers may resend) produce one funnel event.
type DeliveryTransition struct {
	Message        *Message
	FirstDelivered bool
	FirstRead      bool
	FirstFailed    bool
}

// DeliveryRecorder applies delivery receipts to persisted outbound messages.
type DeliveryRecorder interface {
	// ApplyDeliveryStatus stamps delivered_at / read_at / failed_at on the
	// OUTBOUND message with this provider message ID. "read" implies
	// delivered. Returns nil (no error) if no such outbound message exists
	// (e.g. sent before §23A).
	ApplyDeliveryStatus(ctx context.Context, externalMessageID, status string, at time.Time, errDetail string) (*DeliveryTransition, error)
}
