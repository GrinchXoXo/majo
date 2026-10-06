package httpserver

import (
	"net/http"
	"strconv"
	"time"

	"github.com/middleman/backend/internal/funnel"
)

// FunnelHandlers exposes the core marketplace funnel (Phase 3.1 §23A),
// read-only, for the operator dashboard.
type FunnelHandlers struct {
	reader funnel.Reader
	// freshness is the listing freshness window [PARAM] for supply rows.
	freshness time.Duration
}

func NewFunnelHandlers(reader funnel.Reader) *FunnelHandlers {
	return &FunnelHandlers{reader: reader, freshness: 72 * time.Hour}
}

// SetListingFreshness sets the [PARAM] window behind the stale-listing rate.
func (h *FunnelHandlers) SetListingFreshness(d time.Duration) {
	if d > 0 {
		h.freshness = d
	}
}

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
	report := funnel.Report{Since: since, Until: until, Stages: stages}
	// Supply-side rows (Lock V2 §K.2) when the reader provides them.
	if sr, ok := h.reader.(funnel.SupplyReader); ok {
		// Independent of the stages: a supply failure never blanks them.
		if supply, err := sr.Supply(r.Context(), since, until, h.freshness); err != nil {
			report.SupplyError = "failed to compute supply metrics"
		} else {
			report.Supply = supply
		}
	}
	writeJSON(w, http.StatusOK, report)
}
