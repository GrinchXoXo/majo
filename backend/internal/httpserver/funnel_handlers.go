package httpserver

import (
	"net/http"
	"strconv"
	"time"

	"github.com/middleman/backend/internal/funnel"
)

// FunnelHandlers exposes the core marketplace funnel (Phase 3.1 §23A),
// read-only, for the operator dashboard.
type FunnelHandlers struct{ reader funnel.Reader }

func NewFunnelHandlers(reader funnel.Reader) *FunnelHandlers { return &FunnelHandlers{reader: reader} }

// Get handles GET /api/metrics/funnel?days=30 (1-365, default 30).
func (h *FunnelHandlers) Get(w http.ResponseWriter, r *http.Request) {
	days := 30
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 365 {
			writeError(w, http.StatusBadRequest, "days must be between 1 and 365")
			return
		}
		days = n
	}
	until := time.Now().UTC()
	since := until.AddDate(0, 0, -days)
	stages, err := h.reader.Funnel(r.Context(), since, until)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to compute funnel")
		return
	}
	writeJSON(w, http.StatusOK, funnel.Report{Since: since, Until: until, Stages: stages})
}
