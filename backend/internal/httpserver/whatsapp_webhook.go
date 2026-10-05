package httpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"

	"github.com/middleman/backend/infrastructure/whatsapp"
	"github.com/middleman/backend/internal/buyer"
	"github.com/middleman/backend/internal/conversation"
	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/jobs"
	"github.com/middleman/backend/internal/logging"
	"github.com/middleman/backend/internal/request"
	"github.com/middleman/backend/internal/routing"
	"github.com/middleman/backend/internal/vendor"
)

// WhatsAppWebhookHandler receives Meta webhook traffic and, per PRD 1
// §9-11, drives the full Phase 1 loop: find/create buyer and conversation,
// persist the message idempotently, immediately acknowledge the buyer,
// create a BuyerRequest, and run AI extraction in the background.
type WhatsAppWebhookHandler struct {
	provider    whatsapp.MessagingProvider
	convRepo    conversation.Repository
	buyerRepo   buyer.Repository
	vendorRepo  vendor.Repository
	eventWriter event.Writer
	reqService  *request.Service
	routingService *routing.Service
	jobs        *jobs.Runner
	verifyToken string
	logger      *slog.Logger
}

func NewWhatsAppWebhookHandler(
	provider whatsapp.MessagingProvider,
	convRepo conversation.Repository,
	buyerRepo buyer.Repository,
	vendorRepo vendor.Repository,
	eventWriter event.Writer,
	reqService *request.Service,
	routingService *routing.Service,
	jobRunner *jobs.Runner,
	verifyToken string,
	logger *slog.Logger,
) *WhatsAppWebhookHandler {
	return &WhatsAppWebhookHandler{
		provider:    provider,
		convRepo:    convRepo,
		buyerRepo:   buyerRepo,
		vendorRepo:  vendorRepo,
		eventWriter: eventWriter,
		reqService:  reqService,
		routingService: routingService,
		jobs:        jobRunner,
		verifyToken: verifyToken,
		logger:      logger,
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

	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Error("reading webhook body", "error", err)
		w.WriteHeader(http.StatusBadRequest)
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

	exists, err := h.convRepo.MessageExists(ctx, conv.ID, msg.ExternalMessageID)
	if err != nil {
		log.Error("checking message idempotency", "error", err)
		return
	}
	if exists {
		// Duplicate webhook delivery — safe to ignore (PRD 0 FINAL §32,
		// preserved into Phase 1 per PRD 1 §10).
		log.Info("duplicate message ignored", "external_message_id", msg.ExternalMessageID)
		return
	}

	savedMsg, err := h.convRepo.SaveMessage(ctx, &conversation.Message{
		ConversationID:    conv.ID,
		Direction:         conversation.DirectionInbound,
		ExternalMessageID: msg.ExternalMessageID,
		MessageType:       msg.Type,
		Content:           map[string]any{"text": msg.Text},
	})
	if err != nil {
		log.Error("saving inbound message", "error", err)
		return
	}

	if err := h.eventWriter.Write(ctx, &event.Event{
		EventType:  event.MessageReceived,
		EntityType: "conversation",
		EntityID:   &conv.ID,
		ActorType:  event.ActorBuyer,
		Payload: map[string]any{
			"channel": "whatsapp",
			"from":    msg.From,
		},
	}); err != nil {
		log.Error("writing MESSAGE_RECEIVED event", "error", err)
	}

	// Phase 1: only free-text messages become BuyerRequests. Interactive
	// replies (button/list selections) will be handled by later phases
	// (vendor responses, offer selection) once those flows exist.
	if msg.Type != "text" || msg.Text == "" {
		log.Info("non-text inbound message persisted, no request created",
			"conversation_id", conv.ID, "message_type", msg.Type)
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
	if v != nil && v.Status == vendor.StatusActive {
		log.Info("inbound message from known active vendor, routing to vendor pipeline",
			"vendor_id", v.ID, "vendor_name", v.BusinessName)
		h.jobs.Enqueue(func(bgCtx context.Context) {
			h.routingService.ProcessVendorMessage(bgCtx, v, msg.Text)
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
		// Not fatal — the request can still be created; the conversation
		// simply won't show the buyer link until the next message.
	}

	// Immediate acknowledgement (PRD 1 §11): the buyer must not wait for
	// AI processing to know Middleman received their message.
	if err := h.provider.SendText(ctx, msg.From, "Got it — I'm looking into that."); err != nil {
		log.Error("sending immediate acknowledgement", "error", err)
		// Not fatal for request creation — the request still gets created
		// and reviewed even if the ack failed to send.
	} else {
		if err := h.eventWriter.Write(ctx, &event.Event{
			EventType:  event.MessageSent,
			EntityType: "conversation",
			EntityID:   &conv.ID,
			ActorType:  event.ActorSystem,
			Payload:    map[string]any{"kind": "acknowledgement"},
		}); err != nil {
			log.Error("writing MESSAGE_SENT event", "error", err)
		}
	}

	created, err := h.reqService.CreateFromMessage(ctx, conv.ID, b.ID, savedMsg.ID, msg.Text)
	if err != nil {
		log.Error("creating buyer request", "error", err)
		return
	}

	log.Info("buyer request created, queuing extraction",
		"conversation_id", conv.ID,
		"request_id", created.ID,
	)

	// Extraction runs in the background (PRD 1 §12): the webhook response
	// and buyer acknowledgement never wait on the AI service.
	requestID := created.ID
	h.jobs.Enqueue(func(bgCtx context.Context) {
		h.reqService.RunExtraction(bgCtx, requestID)
	})
}
