// Package channel is the channel-neutral outbound message model (Phase 4B,
// Architecture Lock V2 §H). The Presentation layer renders intents into a
// Message; a channel adapter (WhatsApp today) transports it. Business code
// never builds provider payloads.
package channel

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/google/uuid"
)

// Kind is what the message is on the wire.
type Kind string

const (
	KindText     Kind = "text"
	KindImage    Kind = "image"
	KindVideo    Kind = "video"
	KindButtons  Kind = "buttons"  // interactive reply buttons, optional image header
	KindList     Kind = "list"     // interactive list
	KindTemplate Kind = "template" // pre-approved template (outside the 24h window)
)

// MediaRef points at a stored MediaAsset. ProviderRef is filled by a
// MediaPublisher just before sending (the asset is never copied).
type MediaRef struct {
	AssetID     uuid.UUID `json:"asset_id"`
	Kind        string    `json:"kind"` // image | video
	ProviderRef string    `json:"provider_ref,omitempty"`
}

// Button is a reply button. ID is the structured interaction ID.
type Button struct {
	ID    string `json:"id"`
	Title string `json:"title"`
}

// Row is one list row. ID is the structured interaction ID.
type Row struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
}

// Template is a pre-approved template invocation. Names come from
// configuration, never from application logic.
type Template struct {
	Name       string   `json:"name"`
	Language   string   `json:"language"`
	BodyParams []string `json:"body_params,omitempty"`
	// QuickReplyPayloads are the payloads of the template's quick-reply
	// buttons, in button order (structured interaction IDs).
	QuickReplyPayloads []string `json:"quick_reply_payloads,omitempty"`
}

// Message is one outbound message.
type Message struct {
	Kind       Kind      `json:"kind"`
	Body       string    `json:"body,omitempty"`
	Footer     string    `json:"footer,omitempty"`
	Header     *MediaRef `json:"header,omitempty"` // KindButtons: image header
	Media      *MediaRef `json:"media,omitempty"`  // KindImage / KindVideo
	Buttons    []Button  `json:"buttons,omitempty"`
	ListButton string    `json:"list_button,omitempty"`
	Section    string    `json:"section,omitempty"`
	Rows       []Row     `json:"rows,omitempty"`
	Template   *Template `json:"template,omitempty"`
}

// Platform limits for WhatsApp interactive messages [PLATFORM]. Messages
// violating them are rejected before any send.
const (
	MaxButtons        = 3
	MaxButtonTitle    = 20
	MaxRows           = 10
	MaxRowTitle       = 24
	MaxRowDescription = 72
	MaxListButton     = 20
	MaxInteractiveID  = 256
	MaxBody           = 1024
)

// ErrInvalidMessage wraps validation failures.
var ErrInvalidMessage = errors.New("invalid channel message")

// Validate checks the message against the platform limits.
func (m Message) Validate() error {
	bad := func(f string, a ...any) error { return fmt.Errorf("%w: %s", ErrInvalidMessage, fmt.Sprintf(f, a...)) }
	if utf8.RuneCountInString(m.Body) > MaxBody {
		return bad("body longer than %d characters", MaxBody)
	}
	switch m.Kind {
	case KindText:
		if strings.TrimSpace(m.Body) == "" {
			return bad("empty text")
		}
	case KindImage, KindVideo:
		if m.Media == nil {
			return bad("%s without media", m.Kind)
		}
	case KindButtons:
		if len(m.Buttons) == 0 || len(m.Buttons) > MaxButtons {
			return bad("buttons: need 1..%d, got %d", MaxButtons, len(m.Buttons))
		}
		for _, b := range m.Buttons {
			if b.ID == "" || len(b.ID) > MaxInteractiveID || b.Title == "" || utf8.RuneCountInString(b.Title) > MaxButtonTitle {
				return bad("button %q/%q exceeds limits", b.ID, b.Title)
			}
		}
		if strings.TrimSpace(m.Body) == "" {
			return bad("buttons without body")
		}
	case KindList:
		if len(m.Rows) == 0 || len(m.Rows) > MaxRows {
			return bad("list: need 1..%d rows, got %d", MaxRows, len(m.Rows))
		}
		if m.ListButton == "" || utf8.RuneCountInString(m.ListButton) > MaxListButton {
			return bad("list button text %q", m.ListButton)
		}
		for _, r := range m.Rows {
			if r.ID == "" || len(r.ID) > MaxInteractiveID || r.Title == "" || utf8.RuneCountInString(r.Title) > MaxRowTitle ||
				utf8.RuneCountInString(r.Description) > MaxRowDescription {
				return bad("row %q/%q exceeds limits", r.ID, r.Title)
			}
		}
	case KindTemplate:
		if m.Template == nil || m.Template.Name == "" || m.Template.Language == "" {
			return bad("template without name/language")
		}
	default:
		return bad("unknown kind %q", m.Kind)
	}
	return nil
}

// Summary is the human-readable text recorded for the message.
func (m Message) Summary() string {
	switch m.Kind {
	case KindTemplate:
		if m.Template != nil {
			return "[template " + m.Template.Name + "] " + strings.Join(m.Template.BodyParams, " | ")
		}
	case KindList:
		titles := make([]string, len(m.Rows))
		for i, r := range m.Rows {
			titles[i] = r.Title
		}
		return m.Body + "\n[list: " + strings.Join(titles, " / ") + "]"
	case KindButtons:
		titles := make([]string, len(m.Buttons))
		for i, b := range m.Buttons {
			titles[i] = b.Title
		}
		return m.Body + "\n[buttons: " + strings.Join(titles, " / ") + "]"
	}
	return m.Body
}

// InteractionIDs lists every structured ID the message carries.
func (m Message) InteractionIDs() []string {
	var ids []string
	for _, b := range m.Buttons {
		ids = append(ids, b.ID)
	}
	for _, r := range m.Rows {
		ids = append(ids, r.ID)
	}
	if m.Template != nil {
		ids = append(ids, m.Template.QuickReplyPayloads...)
	}
	return ids
}

// Sender transports a rendered message and returns the provider message ID.
type Sender interface {
	SendMessage(ctx context.Context, to string, m Message) (string, error)
}

// MediaUploader uploads bytes to the channel provider for outbound use.
type MediaUploader interface {
	UploadMedia(ctx context.Context, data []byte, mimeType, filename string) (string, error)
}
