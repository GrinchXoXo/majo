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
	control WorkflowControl // Phase 4C hardening: pause / resume one request
	trail   MessageTrail
}

// WorkflowControl pauses and resumes one request's automation.
type WorkflowControl interface {
	Pause(ctx context.Context, buyerRequestID uuid.UUID, by, reason string) (*autonomy.Hold, bool, error)
	Resume(ctx context.Context, buyerRequestID uuid.UUID, by, reason string) (bool, error)
}

// MessageTrail reads what Majo sent (or could not send) for a request.
type MessageTrail interface {
	RequestMessages(ctx context.Context, buyerRequestID uuid.UUID) ([]map[string]any, error)
	Holds(ctx context.Context, buyerRequestID uuid.UUID) ([]*autonomy.Hold, error)
	NoticesForRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*autonomy.Notice, error)
}

// SetControl wires pause/resume and the message trail.
func (h *AutonomyHandlers) SetControl(c WorkflowControl, t MessageTrail) { h.control, h.trail = c, t }

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
			"vendor_clarify_max_rounds": p.VendorClarifyMaxRounds, "retry_max_attempts": p.RetryMaxAttempts, "context_window": p.ContextWindow.String(),
			"recovery_max_rounds": p.RecoveryMaxRounds, "route_max_vendors_per_request": p.MaxVendorsPerRequest,
			"pending_notice_ttl": p.PendingNoticeTTL.String(), "broaden_excluded_categories": p.BroadenExcludedCategories,
			"restricted_terms": len(p.RestrictedTerms), "retry_first_delay_override": p.RetryFirstDelayOverride.String()},
		"templates": map[string]any{"buyer_update_template_configured": false,
			"note": "no approved utility template is configured: updates blocked by the 24h window wait for the buyer's next message"},
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
	if err := h.store.ResolveException(r.Context(), id, actorName(r), strings.TrimSpace(b.Resolution), time.Now().UTC()); err != nil {
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
		ActorType: event.ActorOperator, ActorID: actorID(r), Payload: map[string]any{"resolution": b.Resolution, "by": actorName(r)}})
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
	out := map[string]any{"decisions": ds, "clarifications": cs, "exceptions": ex}
	if h.trail != nil {
		if ms, err := h.trail.RequestMessages(r.Context(), id); err == nil {
			out["messages"] = ms
		}
		if hs, err := h.trail.Holds(r.Context(), id); err == nil {
			out["holds"] = hs
		}
		if ns, err := h.trail.NoticesForRequest(r.Context(), id); err == nil {
			out["pending_notices"] = ns
		}
	}
	writeJSON(w, http.StatusOK, out)
}

// Pause handles POST /api/requests/{id}/pause {"reason": "..."}: Majo takes
// no automatic step for this request until it is resumed.
func (h *AutonomyHandlers) Pause(w http.ResponseWriter, r *http.Request) {
	h.holdAction(w, r, true)
}

// Resume handles POST /api/requests/{id}/resume {"reason": "..."}.
func (h *AutonomyHandlers) Resume(w http.ResponseWriter, r *http.Request) {
	h.holdAction(w, r, false)
}

func (h *AutonomyHandlers) holdAction(w http.ResponseWriter, r *http.Request, pause bool) {
	if h.control == nil {
		writeErrorCode(w, http.StatusNotImplemented, "NOT_CONFIGURED", "workflow holds are not configured")
		return
	}
	id, ok := pathUUID(w, r)
	if !ok {
		return
	}
	var b struct {
		Reason string `json:"reason"`
	}
	if err := decodeStrict(r, &b); err != nil || strings.TrimSpace(b.Reason) == "" {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "reason is required")
		return
	}
	reason := strings.TrimSpace(b.Reason)
	if pause {
		hold, created, err := h.control.Pause(r.Context(), id, actorName(r), reason)
		if err != nil {
			h.logger.ErrorContext(r.Context(), "pausing workflow", "error", err)
			writeErrorCode(w, http.StatusBadRequest, "PAUSE_FAILED", err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"held": true, "already_held": !created, "hold": hold})
		return
	}
	released, err := h.control.Resume(r.Context(), id, actorName(r), reason)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "resuming workflow", "error", err)
		writeErrorCode(w, http.StatusBadRequest, "RESUME_FAILED", err.Error())
		return
	}
	if !released {
		writeErrorCode(w, http.StatusConflict, "NOT_HELD", "this request is not on hold")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"held": false, "resumed": true})
}
