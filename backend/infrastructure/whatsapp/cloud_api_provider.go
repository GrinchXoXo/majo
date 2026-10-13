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
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"github.com/middleman/backend/internal/channel"
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
	// mediaClient downloads inbound media (Phase 4A.1); longer timeout
	// than message sends because a video can be up to 16 MB.
	mediaClient *http.Client
}

func NewCloudAPIProvider(apiURL, accessToken, phoneNumberID, verifyToken, appSecret string) *CloudAPIProvider {
	return &CloudAPIProvider{
		apiURL:        apiURL,
		accessToken:   accessToken,
		phoneNumberID: phoneNumberID,
		verifyToken:   verifyToken,
		appSecret:     appSecret,
		client:        &http.Client{Timeout: 10 * time.Second},
		mediaClient:   &http.Client{Timeout: 2 * time.Minute},
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

// SendMessage implements channel.Sender (Phase 4B): it maps a
// channel-neutral message onto the Cloud API payload for text, image,
// video, reply buttons (optionally with an image header), lists and
// templates. Media must already carry a provider media ID (see
// UploadMedia); nothing is downloaded or re-encoded here.
func (p *CloudAPIProvider) SendMessage(ctx context.Context, to string, m channel.Message) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	payload, err := cloudPayload(to, m)
	if err != nil {
		return "", err
	}
	return p.send(ctx, payload)
}

func mediaObject(ref *channel.MediaRef, caption string) (map[string]any, error) {
	if ref == nil || ref.ProviderRef == "" {
		return nil, fmt.Errorf("%w: media has no provider reference", channel.ErrInvalidMessage)
	}
	obj := map[string]any{"id": ref.ProviderRef}
	if caption != "" {
		obj["caption"] = caption
	}
	return obj, nil
}

func cloudPayload(to string, m channel.Message) (map[string]any, error) {
	payload := map[string]any{"messaging_product": "whatsapp", "recipient_type": "individual", "to": to}
	switch m.Kind {
	case channel.KindText:
		payload["type"] = "text"
		payload["text"] = map[string]any{"body": m.Body}
	case channel.KindImage, channel.KindVideo:
		obj, err := mediaObject(m.Media, m.Body)
		if err != nil {
			return nil, err
		}
		payload["type"] = string(m.Kind)
		payload[string(m.Kind)] = obj
	case channel.KindButtons:
		buttons := make([]map[string]any, len(m.Buttons))
		for i, b := range m.Buttons {
			buttons[i] = map[string]any{"type": "reply", "reply": map[string]string{"id": b.ID, "title": b.Title}}
		}
		inter := map[string]any{"type": "button", "body": map[string]string{"text": m.Body}, "action": map[string]any{"buttons": buttons}}
		if m.Header != nil {
			obj, err := mediaObject(m.Header, "")
			if err != nil {
				return nil, err
			}
			inter["header"] = map[string]any{"type": "image", "image": obj}
		}
		if m.Footer != "" {
			inter["footer"] = map[string]string{"text": m.Footer}
		}
		payload["type"] = "interactive"
		payload["interactive"] = inter
	case channel.KindList:
		rows := make([]map[string]string, len(m.Rows))
		for i, r := range m.Rows {
			row := map[string]string{"id": r.ID, "title": r.Title}
			if r.Description != "" {
				row["description"] = r.Description
			}
			rows[i] = row
		}
		section := map[string]any{"rows": rows}
		if m.Section != "" {
			section["title"] = m.Section
		}
		inter := map[string]any{"type": "list", "body": map[string]string{"text": m.Body},
			"action": map[string]any{"button": m.ListButton, "sections": []any{section}}}
		if m.Footer != "" {
			inter["footer"] = map[string]string{"text": m.Footer}
		}
		payload["type"] = "interactive"
		payload["interactive"] = inter
	case channel.KindTemplate:
		t := m.Template
		var components []map[string]any
		if len(t.BodyParams) > 0 {
			params := make([]map[string]string, len(t.BodyParams))
			for i, v := range t.BodyParams {
				params[i] = map[string]string{"type": "text", "text": v}
			}
			components = append(components, map[string]any{"type": "body", "parameters": params})
		}
		for i, pl := range t.QuickReplyPayloads {
			components = append(components, map[string]any{
				"type": "button", "sub_type": "quick_reply", "index": fmt.Sprint(i),
				"parameters": []map[string]string{{"type": "payload", "payload": pl}},
			})
		}
		tpl := map[string]any{"name": t.Name, "language": map[string]string{"code": t.Language}}
		if len(components) > 0 {
			tpl["components"] = components
		}
		payload["type"] = "template"
		payload["template"] = tpl
	default:
		return nil, fmt.Errorf("%w: unknown kind %q", channel.ErrInvalidMessage, m.Kind)
	}
	return payload, nil
}

// UploadMedia implements channel.MediaUploader: POST /{phone-number-id}/media
// (multipart) returns a media ID usable in outbound messages. Majo uploads a
// stored MediaAsset once and reuses the ID (see the media publisher).
func (p *CloudAPIProvider) UploadMedia(ctx context.Context, data []byte, mimeType, filename string) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("messaging_product", "whatsapp")
	_ = w.WriteField("type", mimeType)
	h := make(textproto.MIMEHeader)
	h.Set("Content-Disposition", fmt.Sprintf(`form-data; name="file"; filename=%q`, filename))
	h.Set("Content-Type", mimeType)
	part, err := w.CreatePart(h)
	if err != nil {
		return "", err
	}
	if _, err := part.Write(data); err != nil {
		return "", err
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("%s/%s/media", p.apiURL, p.phoneNumberID), &buf)
	if err != nil {
		return "", fmt.Errorf("building whatsapp media upload: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+p.accessToken)
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := p.mediaClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("uploading whatsapp media: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return "", fmt.Errorf("whatsapp media upload returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var out struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&out); err != nil || out.ID == "" {
		return "", fmt.Errorf("whatsapp media upload: no media id in response")
	}
	return out.ID, nil
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
						// Context is set when the sender used WhatsApp "Reply"
						// on one of our messages: id is that message's wamid.
						Context *struct {
							From string `json:"from"`
							ID   string `json:"id"`
						} `json:"context"`
						Interactive struct {
							Type        string `json:"type"`
							ButtonReply *struct {
								ID    string `json:"id"`
								Title string `json:"title"`
							} `json:"button_reply"`
							ListReply *struct {
								ID    string `json:"id"`
								Title string `json:"title"`
							} `json:"list_reply"`
						} `json:"interactive"`
						Button *struct {
							Payload string `json:"payload"`
							Text    string `json:"text"`
						} `json:"button"`
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
				if m.Context != nil {
					msg.QuotedMessageID = m.Context.ID
				}
				switch {
				case m.Type == "interactive" && m.Interactive.ButtonReply != nil:
					msg.Interaction = &Interaction{Kind: "button_reply", ID: m.Interactive.ButtonReply.ID, Title: m.Interactive.ButtonReply.Title}
				case m.Type == "interactive" && m.Interactive.ListReply != nil:
					msg.Interaction = &Interaction{Kind: "list_reply", ID: m.Interactive.ListReply.ID, Title: m.Interactive.ListReply.Title}
				case m.Type == "button" && m.Button != nil:
					msg.Interaction = &Interaction{Kind: "quick_reply", ID: m.Button.Payload, Title: m.Button.Text}
				}
				if msg.Interaction != nil {
					msg.Text = msg.Interaction.Title
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
