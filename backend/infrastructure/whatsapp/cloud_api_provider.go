package whatsapp

import (
	"bytes"
	"encoding/json"
	"context"
	"fmt"
	"net/http"
	"time"
)

// CloudAPIProvider implements MessagingProvider against the Meta WhatsApp
// Cloud API directly. Credentials come from environment configuration
// (see internal/config), never hardcoded.
type CloudAPIProvider struct {
	apiURL        string
	accessToken   string
	phoneNumberID string
	verifyToken   string
	client        *http.Client
}

func NewCloudAPIProvider(apiURL, accessToken, phoneNumberID, verifyToken string) *CloudAPIProvider {
	return &CloudAPIProvider{
		apiURL:        apiURL,
		accessToken:   accessToken,
		phoneNumberID: phoneNumberID,
		verifyToken:   verifyToken,
		client:        &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *CloudAPIProvider) SendText(ctx context.Context, to, body string) error {
	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "text",
		"text":              map[string]string{"body": body},
	}
	return p.send(ctx, payload)
}

func (p *CloudAPIProvider) SendInteractive(ctx context.Context, to string, interactive Interactive) error {
	// Phase 0/1: kept intentionally minimal. Full button/list payload
	// construction happens once the buyer-facing conversation flow (Phase 1+)
	// is implemented against real WhatsApp interactive message schemas.
	buttons := make([]map[string]any, 0, len(interactive.Options))
	for i, opt := range interactive.Options {
		buttons = append(buttons, map[string]any{
			"type": "reply",
			"reply": map[string]string{
				"id":    fmt.Sprintf("opt_%d", i),
				"title": opt,
			},
		})
	}

	payload := map[string]any{
		"messaging_product": "whatsapp",
		"to":                to,
		"type":              "interactive",
		"interactive": map[string]any{
			"type": "button",
			"body": map[string]string{"text": interactive.Body},
			"action": map[string]any{
				"buttons": buttons,
			},
		},
	}
	return p.send(ctx, payload)
}

func (p *CloudAPIProvider) send(ctx context.Context, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling whatsapp payload: %w", err)
	}

	url := fmt.Sprintf("%s/%s/messages", p.apiURL, p.phoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("building whatsapp request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return fmt.Errorf("calling whatsapp api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("whatsapp api returned status %d", resp.StatusCode)
	}
	return nil
}

// VerifyWebhookToken implements Meta's webhook verification handshake:
// GET /webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=X&hub.challenge=Y
func (p *CloudAPIProvider) VerifyWebhookToken(mode, token, challenge, expectedToken string) (string, bool) {
	if mode == "subscribe" && token == expectedToken {
		return challenge, true
	}
	return "", false
}

// ParseWebhookPayload extracts inbound messages from a raw Meta webhook body.
// Phase 0 only needs this to exist and not panic on real payloads; full
// parsing of every message type is a Phase 1 concern.
func (p *CloudAPIProvider) ParseWebhookPayload(payload []byte) ([]InboundMessage, error) {
	var raw struct {
		Entry []struct {
			Changes []struct {
				Value struct {
					Messages []struct {
						From      string `json:"from"`
						ID        string `json:"id"`
						Timestamp string `json:"timestamp"`
						Type      string `json:"type"`
						Text      struct {
							Body string `json:"body"`
						} `json:"text"`
					} `json:"messages"`
				} `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}

	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("parsing whatsapp webhook payload: %w", err)
	}

	var messages []InboundMessage
	for _, entry := range raw.Entry {
		for _, change := range entry.Changes {
			for _, m := range change.Value.Messages {
				messages = append(messages, InboundMessage{
					From:              m.From,
					ExternalMessageID: m.ID,
					Type:              m.Type,
					Text:              m.Text.Body,
					Timestamp:         m.Timestamp,
				})
			}
		}
	}

	return messages, nil
}
