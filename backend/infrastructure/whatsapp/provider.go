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
	Type              string // "text", "interactive", "image", "audio", etc.
	Text              string
	Timestamp         string

	// Media is set for image/audio/video/document/sticker messages
	// (Phase 3.1 input foundation). It carries only the provider's opaque
	// media reference — never a download URL — so nothing downstream needs
	// to understand WhatsApp media endpoints. Media is persisted, not yet
	// processed (no download, no transcription).
	Media *Media

	// ProfileName is the sender's own WhatsApp profile name from the
	// webhook's contacts[] block, when Meta supplies it.
	ProfileName string
}

// Media is the WhatsApp-side description of an inbound media attachment.
type Media struct {
	Kind     string // "image" | "audio" | "video" | "document" | "sticker"
	ID       string // Meta media ID (resolvable later via the Graph media endpoint)
	MimeType string
	SHA256   string
	Caption  string
	Filename string
	Voice    bool // true for WhatsApp voice notes (audio recorded in-app)
}

// MessagingProvider is the interface the rest of the backend depends on.
// A future provider (a different BSP, a different channel) implements the
// same interface without requiring changes elsewhere.
type MessagingProvider interface {
	SendText(ctx context.Context, to, body string) error
	SendInteractive(ctx context.Context, to string, interactive Interactive) error
	VerifyWebhookToken(mode, token, challenge, expectedToken string) (string, bool)
	// VerifyWebhookSignature checks Meta's X-Hub-Signature-256 header
	// ("sha256=<hex HMAC-SHA256 of the raw body keyed with the App Secret>").
	// Returns false for a missing, malformed or non-matching signature, and
	// also when no App Secret is configured (fail closed).
	VerifyWebhookSignature(payload []byte, signatureHeader string) bool
	ParseWebhookPayload(payload []byte) ([]InboundMessage, error)
}

// Interactive is a minimal representation of WhatsApp's interactive message
// types (buttons/lists). Per Middleman_Technical_Spec_v1 §1: plain text is
// always the fallback; interactive UI is additive, not required.
type Interactive struct {
	Type    string // "button" | "list"
	Body    string
	Options []string // short option labels, e.g. "View offers", "Pass"
}
