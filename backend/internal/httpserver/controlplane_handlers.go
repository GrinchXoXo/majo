package httpserver

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/controlplane"
)

// Paging defaults for the control-plane read views.
const (
	defaultPageLimit = 50
	maxPageLimit     = 200
)

// pageParams reads ?limit=&offset=. paged reports whether ?limit was given.
func pageParams(r *http.Request) (limit, offset int, paged bool, err error) {
	q := r.URL.Query()
	limit = defaultPageLimit
	if v := q.Get("limit"); v != "" {
		paged = true
		n, e := strconv.Atoi(v)
		if e != nil || n < 1 || n > maxPageLimit {
			return 0, 0, false, errors.New("limit must be between 1 and 200")
		}
		limit = n
	}
	if v := q.Get("offset"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil || n < 0 {
			return 0, 0, false, errors.New("offset must be a non-negative integer")
		}
		offset = n
	}
	return limit, offset, paged, nil
}

// ControlPlaneReader is the read side used by the event timeline and jobs
// pages (implemented by controlplane.ControlPlaneRepository).
type ControlPlaneReader interface {
	ListEvents(ctx context.Context, f controlplane.EventFilter, limit, offset int) ([]controlplane.EventRow, int, error)
	EventTypes(ctx context.Context) ([]string, error)
	ListJobs(ctx context.Context, f controlplane.JobFilter, limit, offset int) ([]controlplane.JobRow, int, error)
	JobSummary(ctx context.Context) ([]controlplane.JobSummaryRow, error)
}

// ControlPlaneHandlers serve GET /api/events, /api/events/types, /api/jobs
// and /api/jobs/summary: read-only views of Majo's own records.
type ControlPlaneHandlers struct {
	repo ControlPlaneReader
}

func NewControlPlaneHandlers(repo ControlPlaneReader) *ControlPlaneHandlers {
	return &ControlPlaneHandlers{repo: repo}
}

var jobStatuses = map[string]bool{"PENDING": true, "RUNNING": true, "SUCCEEDED": true, "FAILED": true}

// Events handles GET /api/events?event_type=&entity_type=&entity_id=&actor_type=&limit=&offset=
func (h *ControlPlaneHandlers) Events(w http.ResponseWriter, r *http.Request) {
	limit, offset, _, err := pageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := r.URL.Query()
	f := controlplane.EventFilter{EventType: q.Get("event_type"), EntityType: q.Get("entity_type"), ActorType: q.Get("actor_type")}
	if v := q.Get("entity_id"); v != "" {
		id, err := uuid.Parse(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "entity_id must be a UUID")
			return
		}
		f.EntityID = &id
	}
	rows, total, err := h.repo.ListEvents(r.Context(), f, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list events")
		return
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "limit": limit, "offset": offset, "events": rows})
}

// EventTypes handles GET /api/events/types.
func (h *ControlPlaneHandlers) EventTypes(w http.ResponseWriter, r *http.Request) {
	types, err := h.repo.EventTypes(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list event types")
		return
	}
	writeJSON(w, http.StatusOK, types)
}

// Jobs handles GET /api/jobs?status=&job_type=&limit=&offset=
func (h *ControlPlaneHandlers) Jobs(w http.ResponseWriter, r *http.Request) {
	limit, offset, _, err := pageParams(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	q := r.URL.Query()
	f := controlplane.JobFilter{Status: q.Get("status"), JobType: q.Get("job_type")}
	if f.Status != "" && !jobStatuses[f.Status] {
		writeError(w, http.StatusBadRequest, "status must be PENDING, RUNNING, SUCCEEDED or FAILED")
		return
	}
	rows, total, err := h.repo.ListJobs(r.Context(), f, limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list jobs")
		return
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "limit": limit, "offset": offset, "jobs": rows})
}

// JobSummary handles GET /api/jobs/summary.
func (h *ControlPlaneHandlers) JobSummary(w http.ResponseWriter, r *http.Request) {
	rows, err := h.repo.JobSummary(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to summarise jobs")
		return
	}
	writeJSON(w, http.StatusOK, rows)
}
