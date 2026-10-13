package httpserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/infrastructure/whatsapp"
	"github.com/middleman/backend/internal/buyer"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/logging"
	"github.com/middleman/backend/internal/marketplace"
	"github.com/middleman/backend/internal/media"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/supply"
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
	deliveries            conversation.DeliveryRecorder
	buyerRepo             buyer.Repository
	vendorRepo            vendor.Repository
	eventWriter           event.Writer
	buyerInbound          *marketplace.BuyerInbound
	routingService        *routing.Service
	jobs                  *jobs.Runner
	media                 media.Enqueuer // Phase 4A.1; nil = vendor media is not ingested
	verifyToken           string
	allowUnsignedWebhooks bool
	logger                *slog.Logger
	failures              conversation.OutboundFailureListener // Phase 4C hardening; nil = receipts only
}

func NewWhatsAppWebhookHandler(
	provider whatsapp.MessagingProvider,
	convRepo conversation.Repository,
	deliveries conversation.DeliveryRecorder,
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
		deliveries:            deliveries,
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

// SetMediaIngestion hands inbound vendor media to durable ingestion
// (Phase 4A.1). The adapter only records a durable job; downloading and
// storing happen in the background worker, never in the webhook request.
func (h *WhatsAppWebhookHandler) SetMediaIngestion(m media.Enqueuer) { h.media = m }

// SetOutboundFailureListener hands provider-reported delivery failures to
// the autonomous runtime, which classifies them like a rejected send
// (Phase 4C hardening). Before this, an asynchronous failure only stamped
// failed_at: a vendor request stayed SENT and offers stayed "delivered".
func (h *WhatsAppWebhookHandler) SetOutboundFailureListener(l conversation.OutboundFailureListener) {
	h.failures = l
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

	// §23A: delivery receipts for messages Majo sent (same signed payload).
	statuses, err := h.provider.ParseStatusUpdates(body)
	if err != nil {
		log.Error("parsing webhook statuses", "error", err)
		return
	}
	for _, st := range statuses {
		h.processStatusUpdate(ctx, st, log)
	}
}

// processStatusUpdate records a delivery receipt and, on the first
// transition to delivered/read/failed, writes one funnel event attributed
// to the business entity the message was about.
func (h *WhatsAppWebhookHandler) processStatusUpdate(ctx context.Context, st whatsapp.StatusUpdate, log *slog.Logger) {
	if h.deliveries == nil {
		return
	}
	at := time.Now().UTC()
	if secs, err := strconv.ParseInt(st.Timestamp, 10, 64); err == nil {
		at = time.Unix(secs, 0).UTC()
	}
	errDetail := ""
	if st.ErrorCode != 0 || st.ErrorTitle != "" {
		errDetail = strconv.Itoa(st.ErrorCode) + " " + st.ErrorTitle
	}

	tr, err := h.deliveries.ApplyDeliveryStatus(ctx, st.MessageID, st.Status, at, errDetail)
	if err != nil {
		log.Error("recording delivery status", "external_message_id", st.MessageID, "status", st.Status, "error", err)
		return
	}
	if tr == nil || tr.Message == nil {
		return // unknown/untracked message, or a status that adds no fact
	}

	entityType, _ := tr.Message.Metadata["entity_type"].(string)
	if entityType == "" {
		entityType = "message"
	}
	var entityID *uuid.UUID
	if s, ok := tr.Message.Metadata["entity_id"].(string); ok {
		if id, err := uuid.Parse(s); err == nil {
			entityID = &id
		}
	}
	if entityID == nil {
		id := tr.Message.ID
		entityID = &id
	}
	payload := event.Merge(tr.Message.Metadata, map[string]any{
		"message_id": tr.Message.ID.String(), "external_message_id": st.MessageID,
		"conversation_id": tr.Message.ConversationID.String(), "status_at": at.Format(time.RFC3339),
		"recipient": st.RecipientID, "error": errDetail, "channel": "whatsapp",
	})
	delete(payload, "entity_type")
	delete(payload, "entity_id")

	write := func(t event.Type) {
		if err := h.eventWriter.Write(ctx, &event.Event{
			EventType: t, EntityType: entityType, EntityID: entityID, ActorType: event.ActorSystem, Payload: payload,
		}); err != nil {
			log.Error("writing delivery event", "type", t, "error", err)
		}
	}
	if tr.FirstDelivered {
		write(event.MessageDelivered)
	}
	if tr.FirstRead {
		write(event.MessageRead)
	}
	if tr.FirstFailed {
		write(event.MessageFailed)
		if h.failures != nil {
			kind, _ := tr.Message.Metadata["kind"].(string)
			text, _ := tr.Message.Content["text"].(string)
			var eid *uuid.UUID
			if s, ok := tr.Message.Metadata["entity_id"].(string); ok {
				if id, err := uuid.Parse(s); err == nil {
					eid = &id
				}
			}
			h.failures.OutboundFailed(ctx, conversation.OutboundFailure{MessageID: tr.Message.ID, ConversationID: tr.Message.ConversationID,
				Kind: kind, EntityType: entityType, EntityID: eid, Metadata: tr.Message.Metadata, Text: text,
				Code: st.ErrorCode, Title: st.ErrorTitle, At: at})
		}
	}
}

func (h *WhatsAppWebhookHandler) processInboundMessage(ctx context.Context, msg whatsapp.InboundMessage, log *slog.Logger) {
	conv, err := h.convRepo.FindOrCreateByExternalID(ctx, "whatsapp", msg.From)
	if err != nil {
		log.Error("finding/creating conversation", "error", err, "from", maskAddress(msg.From))
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
	interactionID := ""
	if msg.Interaction != nil {
		// Phase 4B: the structured interaction ID is persisted with the
		// message before any business decision (classified, then handed off).
		interactionID = msg.Interaction.ID
		content["interaction"] = map[string]any{"kind": msg.Interaction.Kind, "id": msg.Interaction.ID, "title": msg.Interaction.Title}
	}
	metadata := map[string]any{}
	if msg.QuotedMessageID != "" {
		metadata["quoted_message_id"] = msg.QuotedMessageID
	}
	if msg.ProfileName != "" {
		metadata["profile_name"] = msg.ProfileName
	}
	// Phase 4A.2: the vendor's own send time (Meta's timestamp), recorded at
	// creation so supply grouping can order messages by when they were sent.
	sentAt := providerTime(msg.Timestamp)
	if !sentAt.IsZero() {
		metadata["provider_timestamp"] = sentAt.Format(time.RFC3339)
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
		log.Error("checking vendor identity", "error", err, "from", maskAddress(msg.From))
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
			// Vendor media: routing decides whether it is supply content
			// (4A.2), then it is handed to durable ingestion (4A.1,
			// MediaRef -> MEDIA_DOWNLOAD job). No download happens here.
			ref := supply.Inbound{MessageID: savedMsg.ID, ConversationID: conv.ID, At: sentAt}
			// Phase 4A.2: classify first (supply content or not), so the
			// supply candidate exists before its download can finish.
			if mediaRef != nil && h.routingService != nil {
				h.routingService.ProcessVendorMedia(ctx, v, ref, mediaRef.Kind, mediaRef.Caption)
			}
			if mediaRef != nil && h.media != nil {
				if err := h.media.EnqueueDownload(ctx, v.ID, savedMsg.ID, mediaRef); err != nil {
					// The startup backfill re-enqueues vendor media without a job.
					log.Error("enqueueing vendor media download", "error", err, "vendor_id", v.ID, "message_id", savedMsg.ID)
				}
			}
			log.Info("non-text vendor message persisted and handed off", "vendor_id", v.ID, "message_type", msg.Type)
			return
		}
		log.Info("inbound message from known active vendor, routing to vendor pipeline",
			"vendor_id", v.ID, "vendor_name", v.BusinessName)
		vendorRef := v
		text := msg.Text
		ref := supply.Inbound{MessageID: savedMsg.ID, ConversationID: conv.ID, At: sentAt, QuotedProviderMessageID: msg.QuotedMessageID}
		h.jobs.Enqueue(func(bgCtx context.Context) {
			// ProcessVendorMessage notifies the marketplace offer loop
			// (ResponseListener) once the response is persisted.
			h.routingService.ProcessVendorMessageFrom(bgCtx, vendorRef, ref, text)
		})
		return
	}

	b, err := h.buyerRepo.FindOrCreateByExternalID(ctx, "whatsapp", msg.From)
	if err != nil {
		log.Error("finding/creating buyer", "error", err, "from", maskAddress(msg.From))
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
		InteractionID:  interactionID,
	})
}

// providerTime parses Meta's unix-seconds message timestamp ("" or invalid -> zero).
func providerTime(ts string) time.Time {
	secs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || secs <= 0 {
		return time.Time{}
	}
	return time.Unix(secs, 0).UTC()
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

// maskAddress keeps the last 4 digits of a channel address for logs (a
// phone number is personal data; logs are not access-controlled).
func maskAddress(a string) string {
	if len(a) <= 4 {
		return "****"
	}
	return "****" + a[len(a)-4:]
}
