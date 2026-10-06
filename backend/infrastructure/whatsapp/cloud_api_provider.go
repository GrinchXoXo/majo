package whatsapp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
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
	appSecret     string
	client        *http.Client
}

func NewCloudAPIProvider(apiURL, accessToken, phoneNumberID, verifyToken, appSecret string) *CloudAPIProvider {
	return &CloudAPIProvider{
		apiURL:        apiURL,
		accessToken:   accessToken,
		phoneNumberID: phoneNumberID,
		verifyToken:   verifyToken,
		appSecret:     appSecret,
		client:        &http.Client{Timeout: 10 * time.Second},
	}
}

func (p *CloudAPIProvider) SendText(ctx context.Context, to, body string) error {
	_, err := p.SendTextWithID(ctx, to, body)
	return err
}

// SendTextWithID sends a text message and returns Meta's message ID (wamid).
func (p *CloudAPIProvider) SendTextWithID(ctx context.Context, to, body string) (string, error) {
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
	_, err := p.send(ctx, payload)
	return err
}

// send posts a message and returns the wamid from Meta's response
// ({"messages":[{"id":"wamid..."}]}); "" if the response carried none.
func (p *CloudAPIProvider) send(ctx context.Context, payload map[string]any) (string, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling whatsapp payload: %w", err)
	}

	url := fmt.Sprintf("%s/%s/messages", p.apiURL, p.phoneNumberID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building whatsapp request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.accessToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := p.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("calling whatsapp api: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		// Include Meta's error body (bounded): it carries the error code
		// (e.g. 131047 "re-engagement message" when the 24h customer
		// service window has closed and a template is required), which is
		// what the operator needs to see on a failed VendorRequest/Offer.
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("whatsapp api returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var sent struct {
		Messages []struct {
			ID string `json:"id"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&sent); err == nil && len(sent.Messages) > 0 {
		return sent.Messages[0].ID, nil
	}
	return "", nil
}

// VerifyWebhookSignature implements Meta's documented webhook payload
// signature check: X-Hub-Signature-256 = "sha256=" + hex(HMAC-SHA256(raw
// body, App Secret)). Constant-time comparison. Fails closed when no App
// Secret is configured — whether unsigned traffic is tolerated at all is a
// decision made explicitly by the caller (development only), never here.
func (p *CloudAPIProvider) VerifyWebhookSignature(payload []byte, signatureHeader string) bool {
	if p.appSecret == "" {
		return false
	}
	const prefix = "sha256="
	if !strings.HasPrefix(signatureHeader, prefix) {
		return false
	}
	got, err := hex.DecodeString(strings.TrimPrefix(signatureHeader, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(p.appSecret))
	mac.Write(payload)
	return hmac.Equal(got, mac.Sum(nil))
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
// Text bodies, media references (image/audio/video/document/sticker) and the
// sender's profile name are extracted. Media is referenced by Meta media ID
// only; nothing is downloaded here. Status callbacks (sent/delivered/read/
// failed) are not parsed yet.
func (p *CloudAPIProvider) ParseWebhookPayload(payload []byte) ([]InboundMessage, error) {
	type mediaPayload struct {
		ID       string `json:"id"`
		MimeType string `json:"mime_type"`
		SHA256   string `json:"sha256"`
		Caption  string `json:"caption"`
		Filename string `json:"filename"`
		Voice    bool   `json:"voice"`
	}
	var raw struct {
		Entry []struct {
			Changes []struct {
				Value struct {
					Contacts []struct {
						WaID    string `json:"wa_id"`
						Profile struct {
							Name string `json:"name"`
						} `json:"profile"`
					} `json:"contacts"`
					Messages []struct {
						From      string `json:"from"`
						ID        string `json:"id"`
						Timestamp string `json:"timestamp"`
						Type      string `json:"type"`
						Text      struct {
							Body string `json:"body"`
						} `json:"text"`
						Image    *mediaPayload `json:"image"`
						Audio    *mediaPayload `json:"audio"`
						Video    *mediaPayload `json:"video"`
						Document *mediaPayload `json:"document"`
						Sticker  *mediaPayload `json:"sticker"`
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
			profileNames := map[string]string{}
			for _, c := range change.Value.Contacts {
				profileNames[c.WaID] = c.Profile.Name
			}
			for _, m := range change.Value.Messages {
				msg := InboundMessage{
					From:              m.From,
					ExternalMessageID: m.ID,
					Type:              m.Type,
					Text:              m.Text.Body,
					Timestamp:         m.Timestamp,
					ProfileName:       profileNames[m.From],
				}
				for kind, mp := range map[string]*mediaPayload{
					"image": m.Image, "audio": m.Audio, "video": m.Video, "document": m.Document, "sticker": m.Sticker,
				} {
					if mp != nil && mp.ID != "" {
						msg.Media = &Media{
							Kind: kind, ID: mp.ID, MimeType: mp.MimeType, SHA256: mp.SHA256,
							Caption: mp.Caption, Filename: mp.Filename, Voice: mp.Voice,
						}
					}
				}
				messages = append(messages, msg)
			}
		}
	}

	return messages, nil
}

// ParseStatusUpdates extracts Meta delivery-status callbacks
// (entry[].changes[].value.statuses[]) from a webhook body.
func (p *CloudAPIProvider) ParseStatusUpdates(payload []byte) ([]StatusUpdate, error) {
	var raw struct {
		Entry []struct {
			Changes []struct {
				Value struct {
					Statuses []struct {
						ID          string `json:"id"`
						Status      string `json:"status"`
						Timestamp   string `json:"timestamp"`
						RecipientID string `json:"recipient_id"`
						Errors      []struct {
							Code  int    `json:"code"`
							Title string `json:"title"`
						} `json:"errors"`
					} `json:"statuses"`
				} `json:"value"`
			} `json:"changes"`
		} `json:"entry"`
	}
	if err := json.Unmarshal(payload, &raw); err != nil {
		return nil, fmt.Errorf("parsing whatsapp status payload: %w", err)
	}
	var out []StatusUpdate
	for _, e := range raw.Entry {
		for _, c := range e.Changes {
			for _, st := range c.Value.Statuses {
				u := StatusUpdate{MessageID: st.ID, Status: st.Status, RecipientID: st.RecipientID, Timestamp: st.Timestamp}
				if len(st.Errors) > 0 {
					u.ErrorCode, u.ErrorTitle = st.Errors[0].Code, st.Errors[0].Title
				}
				out = append(out, u)
			}
		}
	}
	return out, nil
}
