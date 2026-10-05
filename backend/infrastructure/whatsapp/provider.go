// Package whatsapp implements the MessagingProvider boundary for the Meta
// WhatsApp Cloud API (per Middleman_Technical_Spec_v1 §1: Cloud API directly,
// no BSP for MVP). Business logic elsewhere in the codebase depends only on
// the MessagingProvider interface, never on this package's types directly.
package whatsapp

import "context"

// InboundMessage is what the webhook handler parses out of a raw Meta payload.
type InboundMessage struct {
	From              string // WhatsApp ID / phone number of the sender
	ExternalMessageID string // Meta's message ID, used for idempotency
	Type              string // "text", "interactive", "image", etc.
	Text              string
	Timestamp         string
}

// MessagingProvider is the interface the rest of the backend depends on.
// A future provider (a different BSP, a different channel) implements the
// same interface without requiring changes elsewhere.
type MessagingProvider interface {
	SendText(ctx context.Context, to, body string) error
	SendInteractive(ctx context.Context, to string, interactive Interactive) error
	VerifyWebhookToken(mode, token, challenge, expectedToken string) (string, bool)
	ParseWebhookPayload(payload []byte) ([]InboundMessage, error)
}

// Interactive is a minimal representation of WhatsApp's interactive message
// types (buttons/lists). Per Middleman_Technical_Spec_v1 §1: plain text is
// always the fallback; interactive UI is additive, not required.
type Interactive struct {
	Type    string   // "button" | "list"
	Body    string
	Options []string // short option labels, e.g. "View offers", "Pass"
}
