package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/middleman/backend/infrastructure/whatsapp"
	"github.com/middleman/backend/internal/buyer"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/logging"
	"github.com/middleman/backend/internal/marketplace"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/vendor"
)

// maxWebhookBody bounds how much of a webhook body is read (Meta payloads
// are small; this prevents unbounded reads from unauthenticated callers).
const maxWebhookBody = 1 << 20 // 1 MiB

// WhatsAppWebhookHandler is the WhatsApp channel adapter. Since Phase 3.1 it
// does only channel work — verify, parse, persist, de-duplicate, identify
// the sender — and hands every buyer message to marketplace.BuyerInbound,
// which owns context resolution (new request vs. offer selection). Vendor
// messages still go to routing.ProcessVendorMessage (unchanged).
type WhatsAppWebhookHandler struct {
	provider              whatsapp.MessagingProvider
	convRepo              conversation.Repository
	buyerRepo             buyer.Repository
	vendorRepo            vendor.Repository
	eventWriter           event.Writer
	buyerInbound          *marketplace.BuyerInbound
	routingService        *routing.Service
	jobs                  *jobs.Runner
	verifyToken           string
	allowUnsignedWebhooks bool
	logger                *slog.Logger
}

func NewWhatsAppWebhookHandler(
	provider whatsapp.MessagingProvider,
	convRepo conversation.Repository,
	buyerRepo buyer.Repository,
	vendorRepo vendor.Repository,
	eventWriter event.Writer,
	buyerInbound *marketplace.BuyerInbound,
	routingService *routing.Service,
	jobRunner *jobs.Runner,
	verifyToken string,
	allowUnsignedWebhooks bool,
	logger *slog.Logger,
) *WhatsAppWebhookHandler {
	return &WhatsAppWebhookHandler{
		provider:              provider,
		convRepo:              convRepo,
		buyerRepo:             buyerRepo,
		vendorRepo:            vendorRepo,
		eventWriter:           eventWriter,
		buyerInbound:          buyerInbound,
		routingService:        routingService,
		jobs:                  jobRunner,
		verifyToken:           verifyToken,
		allowUnsignedWebhooks: allowUnsignedWebhooks,
		logger:                logger,
	}
}

// ServeHTTP handles both the Meta verification handshake (GET) and
// incoming message webhooks (POST).
func (h *WhatsAppWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		h.handleVerification(w, r)
	case http.MethodPost:
		h.handleIncoming(w, r)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (h *WhatsAppWebhookHandler) handleVerification(w http.ResponseWriter, r *http.Request) {
	mode := r.URL.Query().Get("hub.mode")
	token := r.URL.Query().Get("hub.verify_token")
	challenge := r.URL.Query().Get("hub.challenge")

	if resp, ok := h.provider.VerifyWebhookToken(mode, token, challenge, h.verifyToken); ok {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(resp))
		return
	}

	w.WriteHeader(http.StatusForbidden)
}

func (h *WhatsAppWebhookHandler) handleIncoming(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logging.FromContext(ctx, h.logger)

	body, err := io.ReadAll(io.LimitReader(r.Body, maxWebhookBody))
	if err != nil {
		log.Error("reading webhook body", "error", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	// Phase 3.1 §3A-1: authenticate the payload with Meta's documented
	// X-Hub-Signature-256 HMAC before anything is persisted or acted on.
	// Unsigned traffic is accepted ONLY in development with no App Secret
	// configured (config.AllowUnsignedWebhooks), for local simulation.
	if !h.allowUnsignedWebhooks && !h.provider.VerifyWebhookSignature(body, r.Header.Get("X-Hub-Signature-256")) {
		log.Warn("rejected webhook with missing or invalid signature", "remote_addr", r.RemoteAddr)
		w.WriteHeader(http.StatusUnauthorized)
		return
	}

	// Acknowledge Meta immediately regardless of downstream processing
	// outcome — Meta will retry on non-2xx responses, and slow/failed
	// downstream processing must never turn into duplicate deliveries.
	w.WriteHeader(http.StatusOK)

	messages, err := h.provider.ParseWebhookPayload(body)
	if err != nil {
		log.Error("parsing webhook payload", "error", err)
		return
	}

	for _, msg := range messages {
		h.processInboundMessage(ctx, msg, log)
	}
}

func (h *WhatsAppWebhookHandler) processInboundMessage(ctx context.Context, msg whatsapp.InboundMessage, log *slog.Logger) {
	conv, err := h.convRepo.FindOrCreateByExternalID(ctx, "whatsapp", msg.From)
	if err != nil {
		log.Error("finding/creating conversation", "error", err, "from", msg.From)
		return
	}

	// Fast-path duplicate check; SaveMessage below is the authoritative,
	// race-safe gate (ErrDuplicateMessage).
	exists, err := h.convRepo.MessageExists(ctx, conv.ID, msg.ExternalMessageID)
	if err != nil {
		log.Error("checking message idempotency", "error", err)
		return
	}
	if exists {
		log.Info("duplicate message ignored", "external_message_id", msg.ExternalMessageID)
		return
	}

	mediaRef := toMediaRef(msg.Media)
	content := map[string]any{"text": msg.Text}
	if mediaRef != nil {
		content["media"] = mediaRef
	}
	metadata := map[string]any{}
	if msg.ProfileName != "" {
		metadata["profile_name"] = msg.ProfileName
	}

	savedMsg, err := h.convRepo.SaveMessage(ctx, &conversation.Message{
		ConversationID:    conv.ID,
		Direction:         conversation.DirectionInbound,
		ExternalMessageID: msg.ExternalMessageID,
		MessageType:       msg.Type,
		Content:           content,
		Metadata:          metadata,
	})
	if errors.Is(err, conversation.ErrDuplicateMessage) {
		log.Info("duplicate message ignored (concurrent delivery)", "external_message_id", msg.ExternalMessageID)
		return
	}
	if err != nil {
		log.Error("saving inbound message", "error", err)
		return
	}

	// Phase 2: check whether this sender is a known vendor BEFORE assuming
	// they're a buyer (PRD 2 §9-10). An unrecognized number never silently
	// becomes a vendor — it only takes the buyer path.
	v, err := h.vendorRepo.FindByWhatsAppID(ctx, msg.From)
	if err != nil {
		log.Error("checking vendor identity", "error", err, "from", msg.From)
		return
	}
	isVendor := v != nil && v.Status == vendor.StatusActive

	actor := event.ActorBuyer
	if isVendor {
		actor = event.ActorVendor
	}
	if err := h.eventWriter.Write(ctx, &event.Event{
		EventType:  event.MessageReceived,
		EntityType: "conversation",
		EntityID:   &conv.ID,
		ActorType:  actor,
		Payload: map[string]any{
			"channel": "whatsapp", "from": msg.From, "message_type": msg.Type, "has_media": mediaRef != nil,
		},
	}); err != nil {
		log.Error("writing MESSAGE_RECEIVED event", "error", err)
	}

	if isVendor {
		if msg.Type != "text" || msg.Text == "" {
			// Vendor media (e.g. product photos) is persisted with its
			// MediaRef but not yet processed.
			log.Info("non-text vendor message persisted, not processed", "vendor_id", v.ID, "message_type", msg.Type)
			return
		}
		log.Info("inbound message from known active vendor, routing to vendor pipeline",
			"vendor_id", v.ID, "vendor_name", v.BusinessName)
		vendorRef := v
		text := msg.Text
		h.jobs.Enqueue(func(bgCtx context.Context) {
			// ProcessVendorMessage notifies the marketplace offer loop
			// (ResponseListener) once the response is persisted.
			h.routingService.ProcessVendorMessage(bgCtx, vendorRef, text)
		})
		return
	}

	b, err := h.buyerRepo.FindOrCreateByExternalID(ctx, "whatsapp", msg.From)
	if err != nil {
		log.Error("finding/creating buyer", "error", err, "from", msg.From)
		return
	}
	if err := h.convRepo.LinkBuyer(ctx, conv.ID, b.ID); err != nil {
		log.Error("linking buyer to conversation", "error", err)
		// Not fatal — the conversation simply won't show the buyer link
		// until the next message.
	}

	h.buyerInbound.Handle(ctx, marketplace.InboundBuyerMessage{
		ConversationID: conv.ID,
		BuyerID:        b.ID,
		MessageID:      savedMsg.ID,
		ReplyTo:        msg.From,
		Type:           msg.Type,
		Text:           msg.Text,
		Media:          mediaRef,
	})
}

// toMediaRef maps the WhatsApp-specific media description onto the
// channel-agnostic conversation.MediaRef.
func toMediaRef(m *whatsapp.Media) *conversation.MediaRef {
	if m == nil {
		return nil
	}
	return &conversation.MediaRef{
		Kind: m.Kind, Provider: "whatsapp", ProviderRef: m.ID, MimeType: m.MimeType,
		SHA256: m.SHA256, Caption: m.Caption, Filename: m.Filename, Voice: m.Voice,
	}
}
