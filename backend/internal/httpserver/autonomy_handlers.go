package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/autonomy"
	"github.com/middleman/backend/internal/event"
)

// Phase 4C correction: the control plane of the autonomous runtime. Every
// route is operator-only. Operators observe, inspect decisions and handle
// exceptions here; they do not process normal transactions.
//
//	GET  /api/autonomy/settings                 flags + parameters
//	GET  /api/autonomy/metrics?since=RFC3339     autonomy metrics (raw counts + %)
//	GET  /api/autonomy/exceptions?status=OPEN    operator exception queue
//	POST /api/autonomy/exceptions/{id}/resolve   {"resolution": "..."}
//	GET  /api/requests/{id}/autonomy            decisions, clarifications, exceptions for one request
type AutonomyHandlers struct {
	flags   autonomy.Flags
	params  autonomy.Params
	store   autonomy.Store
	metrics MetricsReader
	events  event.Writer
	logger  *slog.Logger
	advance Readvancer
}

// Readvancer hands a request back to Majo once its exception is resolved.
type Readvancer interface {
	Readvance(ctx context.Context, id uuid.UUID)
}

// SetReadvancer wires the workflow engine.
func (h *AutonomyHandlers) SetReadvancer(r Readvancer) { h.advance = r }

// MetricsReader computes the autonomy report.
type MetricsReader interface {
	Metrics(ctx context.Context, since *time.Time) (*autonomy.Metrics, error)
}

func NewAutonomyHandlers(flags autonomy.Flags, params autonomy.Params, store autonomy.Store, metrics MetricsReader, events event.Writer, logger *slog.Logger) *AutonomyHandlers {
	return &AutonomyHandlers{flags: flags, params: params, store: store, metrics: metrics, events: events, logger: logger}
}

// Settings handles GET /api/autonomy/settings.
func (h *AutonomyHandlers) Settings(w http.ResponseWriter, r *http.Request) {
	p := h.params
	writeJSON(w, http.StatusOK, map[string]any{
		"operating_model": "autonomous by default; operators are control-plane participants and exception handlers",
		"flags":           h.flags.Map(), "policy_version": autonomy.PolicyVersion,
		"params": map[string]any{"confirm_min_confidence": p.ConfirmMinConfidence, "max_clarifications": p.MaxClarifications,
			"clarification_ttl": p.ClarificationTTL.String(), "route_min_score": p.RouteMinScore, "route_max_vendors": p.RouteMaxVendors,
			"route_vendor_max_open": p.RouteVendorMaxOpen, "vendor_request_ttl": p.VendorRequestTTL.String(),
			"vendor_clarify_max_rounds": p.VendorClarifyMaxRounds, "retry_max_attempts": p.RetryMaxAttempts, "context_window": p.ContextWindow.String()},
	})
}

// Metrics handles GET /api/autonomy/metrics?since=.
func (h *AutonomyHandlers) Metrics(w http.ResponseWriter, r *http.Request) {
	var since *time.Time
	if v := r.URL.Query().Get("since"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "since must be RFC3339")
			return
		}
		since = &t
	}
	m, err := h.metrics.Metrics(r.Context(), since)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "autonomy metrics", "error", err)
		writeErrorCode(w, http.StatusInternalServerError, "INTERNAL", "metrics failed")
		return
	}
	writeJSON(w, http.StatusOK, m)
}

// Exceptions handles GET /api/autonomy/exceptions?status=OPEN|RESOLVED.
func (h *AutonomyHandlers) Exceptions(w http.ResponseWriter, r *http.Request) {
	status := strings.ToUpper(r.URL.Query().Get("status"))
	if status != "" && status != autonomy.ExceptionOpen && status != autonomy.ExceptionResolved {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "status must be OPEN or RESOLVED")
		return
	}
	es, err := h.store.ListExceptions(r.Context(), status, 200)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "listing exceptions", "error", err)
		writeErrorCode(w, http.StatusInternalServerError, "INTERNAL", "listing failed")
		return
	}
	writeJSON(w, http.StatusOK, es)
}

// ResolveException handles POST /api/autonomy/exceptions/{id}/resolve.
func (h *AutonomyHandlers) ResolveException(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var b struct {
		Resolution string `json:"resolution"`
	}
	if err := decodeStrict(r, &b); err != nil || strings.TrimSpace(b.Resolution) == "" {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "resolution is required")
		return
	}
	if err := h.store.ResolveException(r.Context(), id, "operator", strings.TrimSpace(b.Resolution), time.Now().UTC()); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeErrorCode(w, http.StatusNotFound, "NOT_FOUND", "no open exception with this id")
			return
		}
		writeErrorCode(w, http.StatusInternalServerError, "INTERNAL", "resolve failed")
		return
	}
	if h.advance != nil {
		if all, err := h.store.ListExceptions(r.Context(), autonomy.ExceptionResolved, 500); err == nil {
			for _, e := range all {
				if e.ID == id && e.BuyerRequestID != nil {
					h.advance.Readvance(r.Context(), *e.BuyerRequestID) // Majo continues from here
				}
			}
		}
	}
	eid := id
	_ = h.events.Write(r.Context(), &event.Event{EventType: event.WorkflowExceptionResolved, EntityType: "workflow_exception", EntityID: &eid,
		ActorType: event.ActorOperator, Payload: map[string]any{"resolution": b.Resolution}})
	writeJSON(w, http.StatusOK, map[string]any{"resolved": true})
}

// Request handles GET /api/requests/{id}/autonomy.
func (h *AutonomyHandlers) Request(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "invalid id")
		return
	}
	ds, err := h.store.DecisionsForRequest(r.Context(), id)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "INTERNAL", "loading decisions failed")
		return
	}
	cs, err := h.store.ClarificationsForRequest(r.Context(), id)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "INTERNAL", "loading clarifications failed")
		return
	}
	all, err := h.store.ListExceptions(r.Context(), "", 500)
	if err != nil {
		writeErrorCode(w, http.StatusInternalServerError, "INTERNAL", "loading exceptions failed")
		return
	}
	ex := []*autonomy.Exception{}
	for _, e := range all {
		if e.BuyerRequestID != nil && *e.BuyerRequestID == id {
			ex = append(ex, e)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"decisions": ds, "clarifications": cs, "exceptions": ex})
}
