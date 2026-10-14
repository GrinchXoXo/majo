// Package conversation owns WhatsApp (and future channel) conversation state.
// Per PRD 0.2 §28: WhatsApp itself is not treated as the source of truth for
// conversation state — Majo maintains its own record of what it is
// currently waiting for.
package conversation

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
)

// Status represents what Majo is currently doing within a conversation.
// This is intentionally small in Phase 0/1; more states are added as the
// request lifecycle (Phase 1) and offer lifecycle (later phases) are built.
type Status string

const (
	StatusActive Status = "ACTIVE"
	StatusClosed Status = "CLOSED"
)

// Conversation is a channel-agnostic thread with a single external party.
type Conversation struct {
	ID         uuid.UUID
	Channel    string // "whatsapp" initially; kept generic on purpose
	ExternalID string // e.g. WhatsApp wa_id
	BuyerID    *uuid.UUID
	Status     Status
	Metadata   map[string]any
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// Direction distinguishes inbound (from user) vs outbound (from Majo) messages.
type Direction string

const (
	DirectionInbound  Direction = "INBOUND"
	DirectionOutbound Direction = "OUTBOUND"
)

// Message is a single message within a conversation.
type Message struct {
	ID                uuid.UUID
	ConversationID    uuid.UUID
	Direction         Direction
	ExternalMessageID string // used for webhook idempotency
	MessageType       string
	Content           map[string]any
	Metadata          map[string]any
	CreatedAt         time.Time
}

// ErrDuplicateMessage is returned by SaveMessage when a message with the
// same (conversation, external message ID) already exists. It is the
// race-safe idempotency gate for webhook redelivery: MessageExists is a
// fast-path check, but only the insert itself can be authoritative when two
// deliveries of the same message are processed concurrently.
var ErrDuplicateMessage = errors.New("conversation: duplicate external message")

// MediaRef is a channel-agnostic reference to an inbound media attachment
// (Phase 3.1 input foundation). It records WHAT was received and WHERE the
// channel provider can resolve it (Provider + ProviderRef, e.g. a WhatsApp
// media ID) — never a provider download URL, which is short-lived and
// channel-specific. Persisted in Message.Content["media"]; not yet
// downloaded, stored or transcribed.
type MediaRef struct {
	Kind        string `json:"kind"`         // audio | image | video | document | sticker
	Provider    string `json:"provider"`     // e.g. "whatsapp"
	ProviderRef string `json:"provider_ref"` // provider's opaque media ID
	MimeType    string `json:"mime_type,omitempty"`
	SHA256      string `json:"sha256,omitempty"`
	Caption     string `json:"caption,omitempty"`
	Filename    string `json:"filename,omitempty"`
	Voice       bool   `json:"voice,omitempty"` // a recorded voice note, not an audio file
}

// Repository is the persistence boundary for conversations/messages.
// Kept as an interface so the domain logic never depends on pgx directly.
type Repository interface {
	FindOrCreateByExternalID(ctx context.Context, channel, externalID string) (*Conversation, error)
	FindByID(ctx context.Context, id uuid.UUID) (*Conversation, error)
	// SaveMessage persists a message. Returns ErrDuplicateMessage if the
	// (conversation, external message ID) pair already exists.
	SaveMessage(ctx context.Context, msg *Message) (*Message, error)
	// MessageExists supports idempotent webhook processing: if a WhatsApp
	// message ID has already been persisted, the webhook handler can safely
	// no-op on redelivery.
	MessageExists(ctx context.Context, conversationID uuid.UUID, externalMessageID string) (bool, error)
	// LinkBuyer associates a conversation with a buyer identity (Phase 1).
	// Safe to call repeatedly with the same buyer ID.
	LinkBuyer(ctx context.Context, conversationID, buyerID uuid.UUID) error
}
