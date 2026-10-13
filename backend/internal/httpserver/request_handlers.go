package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/event"
	"github.com/middleman/backend/internal/request"
)

// RequestHandlers implements the operator-facing request API from PRD 1 §31:
//
//	GET    /api/requests
//	GET    /api/requests/:id
//	PATCH  /api/requests/:id
//	POST   /api/requests/:id/confirm
//	POST   /api/requests/:id/clarify
//	GET    /api/requests/:id/events
type RequestHandlers struct {
	requests  request.Repository
	service   *request.Service
	events    event.Reader
	clarifier Clarifier
	pager     RequestPager
}

// RequestPager serves the paginated request list (?limit=&offset=), with the
// total in X-Total-Count. Without ?limit the full list is returned as before.
type RequestPager interface {
	ListPage(ctx context.Context, status *request.Status, limit, offset int) ([]*request.BuyerRequest, error)
	CountRequests(ctx context.Context, status string) (int, error)
}

// SetPager enables ?limit=&offset= on GET /api/requests.
func (h *RequestHandlers) SetPager(p RequestPager) { h.pager = p }

// Clarifier records an operator question as an open clarification so the
// buyer's answer continues the request (workflow.Engine).
type Clarifier interface {
	OperatorClarify(ctx context.Context, requestID uuid.UUID, question string) error
}

// SetClarifier wires the autonomous runtime's clarification state.
func (h *RequestHandlers) SetClarifier(c Clarifier) { h.clarifier = c }

func NewRequestHandlers(requests request.Repository, service *request.Service, events event.Reader) *RequestHandlers {
	return &RequestHandlers{requests: requests, service: service, events: events}
}

// requestDTO is the wire format returned to the dashboard. Kept separate
// from the domain type so internal representation can evolve independently
// of the API contract.
type requestDTO struct {
	ID             string         `json:"id"`
	ConversationID string         `json:"conversation_id"`
	BuyerID        string         `json:"buyer_id"`
	Status         string         `json:"status"`
	Category       *string        `json:"category"`
	Product        *string        `json:"product"`
	Variant        *string        `json:"variant"`
	BudgetMin      *int64         `json:"budget_min"`
	BudgetMax      *int64         `json:"budget_max"`
	Currency       string         `json:"currency"`
	Location       *string        `json:"location"`
	Condition      *string        `json:"condition"`
	Quantity       *int           `json:"quantity"`
	Urgency        *string        `json:"urgency"`
	Preferences    []string       `json:"preferences"`
	RawMessage     string         `json:"raw_message"`
	ExtractedData  map[string]any `json:"extracted_data"`
	Confidence     float64        `json:"confidence"`
	Completeness   string         `json:"completeness"`
	CreatedAt      string         `json:"created_at"`
	UpdatedAt      string         `json:"updated_at"`
	ConfirmedAt    *string        `json:"confirmed_at"`
	// Phase 4B demand bridge (attribution).
	Origin              string  `json:"origin"`
	ListingID           *string `json:"listing_id"`
	DiscoveryDeliveryID *string `json:"discovery_delivery_id"`
}

func toDTO(r *request.BuyerRequest) requestDTO {
	dto := requestDTO{
		Origin:              string(r.Origin),
		ListingID:           idString(r.ListingID),
		DiscoveryDeliveryID: idString(r.DiscoveryDeliveryID),
		ID:                  r.ID.String(),
		ConversationID:      r.ConversationID.String(),
		BuyerID:             r.BuyerID.String(),
		Status:              string(r.Status),
		Category:            r.Category,
		Product:             r.Product,
		Variant:             r.Variant,
		BudgetMin:           r.BudgetMin,
		BudgetMax:           r.BudgetMax,
		Currency:            r.Currency,
		Location:            r.Location,
		Condition:           r.Condition,
		Quantity:            r.Quantity,
		Urgency:             r.Urgency,
		Preferences:         r.Preferences,
		RawMessage:          r.RawMessage,
		ExtractedData:       r.ExtractedData,
		Confidence:          r.Confidence,
		Completeness:        string(r.Completeness),
		CreatedAt:           r.CreatedAt.Format(timeFormat),
		UpdatedAt:           r.UpdatedAt.Format(timeFormat),
	}
	if r.ConfirmedAt != nil {
		s := r.ConfirmedAt.Format(timeFormat)
		dto.ConfirmedAt = &s
	}
	return dto
}

const timeFormat = "2006-01-02T15:04:05Z07:00"

// List handles GET /api/requests?status=NEEDS_REVIEW
func (h *RequestHandlers) List(w http.ResponseWriter, r *http.Request) {
	var statusFilter *request.Status
	if s := r.URL.Query().Get("status"); s != "" {
		st := request.Status(s)
		statusFilter = &st
	}

	var reqs []*request.BuyerRequest
	var err error
	if limit, offset, paged, perr := pageParams(r); perr != nil {
		writeError(w, http.StatusBadRequest, perr.Error())
		return
	} else if paged && h.pager != nil {
		total, cerr := h.pager.CountRequests(r.Context(), r.URL.Query().Get("status"))
		if cerr != nil {
			writeError(w, http.StatusInternalServerError, "failed to count requests")
			return
		}
		w.Header().Set("X-Total-Count", strconv.Itoa(total))
		reqs, err = h.pager.ListPage(r.Context(), statusFilter, limit, offset)
	} else {
		reqs, err = h.requests.List(r.Context(), statusFilter)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list requests")
		return
	}

	dtos := make([]requestDTO, 0, len(reqs))
	for _, req := range reqs {
		dtos = append(dtos, toDTO(req))
	}

	writeJSON(w, http.StatusOK, dtos)
}

// Get handles GET /api/requests/:id
func (h *RequestHandlers) Get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}

	req, err := h.requests.FindByID(r.Context(), id)
	if err != nil {
		writeError(w, http.StatusNotFound, "request not found")
		return
	}

	writeJSON(w, http.StatusOK, toDTO(req))
}

// patchRequestBody mirrors request.Fields but with JSON tags; only
// provided (non-null) fields are applied (PRD 1 §31-32).
type patchRequestBody struct {
	Category    *string  `json:"category"`
	Product     *string  `json:"product"`
	Variant     *string  `json:"variant"`
	BudgetMin   *int64   `json:"budget_min"`
	BudgetMax   *int64   `json:"budget_max"`
	Location    *string  `json:"location"`
	Condition   *string  `json:"condition"`
	Quantity    *int     `json:"quantity"`
	Urgency     *string  `json:"urgency"`
	Preferences []string `json:"preferences"`
}

// Patch handles PATCH /api/requests/:id — the operator correction endpoint.
func (h *RequestHandlers) Patch(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}

	var body patchRequestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	fields := request.Fields{
		Category:    body.Category,
		Product:     body.Product,
		Variant:     body.Variant,
		BudgetMin:   body.BudgetMin,
		BudgetMax:   body.BudgetMax,
		Location:    body.Location,
		Condition:   body.Condition,
		Quantity:    body.Quantity,
		Urgency:     body.Urgency,
		Preferences: body.Preferences,
	}

	// Phase 1 does not implement operator authentication; actorID is nil
	// (system-attributed). See PRD 1 §42 — production auth is a documented
	// follow-up, not a Phase 1 blocker.
	updated, err := h.service.Correct(r.Context(), id, fields, actorID(r))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to apply correction")
		return
	}

	writeJSON(w, http.StatusOK, toDTO(updated))
}

// Confirm handles POST /api/requests/:id/confirm
func (h *RequestHandlers) Confirm(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}

	confirmed, err := h.service.Confirm(r.Context(), id, actorID(r))
	if errors.Is(err, request.ErrInvalidTransition) {
		writeError(w, http.StatusConflict, "request can only be confirmed from NEEDS_REVIEW (extraction may still be running)")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to confirm request")
		return
	}

	writeJSON(w, http.StatusOK, toDTO(confirmed))
}

type clarifyBody struct {
	Question string `json:"question"`
}

// Clarify handles POST /api/requests/:id/clarify
func (h *RequestHandlers) Clarify(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}

	var body clarifyBody
	_ = json.NewDecoder(r.Body).Decode(&body) // question is optional; empty uses the default

	clarify := h.service.Clarify
	if h.clarifier != nil {
		clarify = h.clarifier.OperatorClarify
	}
	if err := clarify(r.Context(), id, body.Question); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to send clarification")
		return
	}

	writeJSON(w, http.StatusOK, map[string]string{"status": "sent"})
}

// Events handles GET /api/requests/:id/events
func (h *RequestHandlers) Events(w http.ResponseWriter, r *http.Request) {
	id, ok := parseUUIDParam(w, r)
	if !ok {
		return
	}

	evts, err := h.events.ListByEntity(r.Context(), "buyer_request", id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load events")
		return
	}

	type eventDTO struct {
		EventType string         `json:"event_type"`
		ActorType string         `json:"actor_type"`
		Payload   map[string]any `json:"payload"`
		CreatedAt string         `json:"created_at"`
	}

	dtos := make([]eventDTO, 0, len(evts))
	for _, e := range evts {
		dtos = append(dtos, eventDTO{
			EventType: string(e.EventType),
			ActorType: string(e.ActorType),
			Payload:   e.Payload,
			CreatedAt: e.CreatedAt.Format(timeFormat),
		})
	}

	writeJSON(w, http.StatusOK, dtos)
}

// --- shared helpers ---

func parseUUIDParam(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	idStr := r.PathValue("id")
	id, err := uuid.Parse(idStr)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return uuid.Nil, false
	}
	return id, true
}

func parseUUID(s string) (uuid.UUID, error) {
	return uuid.Parse(s)
}

func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func idString(id *uuid.UUID) *string {
	if id == nil {
		return nil
	}
	s := id.String()
	return &s
}
