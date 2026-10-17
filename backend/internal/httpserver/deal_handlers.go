package httpserver

// Pilot batch item 5: read-only deal views for the dashboard (the free-deal
// counter and the follow-up outcomes). Nothing here changes a deal.

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/middleman/backend/internal/deal"
)

// DealReader is implemented by deal.Service.
type DealReader interface {
	Summary(ctx context.Context) (*deal.Summary, error)
	List(ctx context.Context, status string, limit, offset int) ([]*deal.Deal, int, error)
	Config() deal.Config
}

type DealHandlers struct {
	deals  DealReader
	logger *slog.Logger
}

func NewDealHandlers(d DealReader, logger *slog.Logger) *DealHandlers {
	return &DealHandlers{deals: d, logger: logger}
}

var dealStatuses = map[string]bool{"": true, "PENDING": true, "ASKED": true, "CONFIRMED": true, "BUYER_ONLY": true,
	"VENDOR_ONLY": true, "DECLINED": true, "DISPUTED": true, "NO_ANSWER": true}

// Summary handles GET /api/deals/summary.
func (h *DealHandlers) Summary(w http.ResponseWriter, r *http.Request) {
	s, err := h.deals.Summary(r.Context())
	if err != nil {
		h.logger.ErrorContext(r.Context(), "deal summary", "error", err)
		writeErrorCode(w, http.StatusInternalServerError, "INTERNAL", "could not read deals")
		return
	}
	c := h.deals.Config()
	writeJSON(w, http.StatusOK, map[string]any{"summary": s, "settings": map[string]any{
		"follow_up": c.Enabled, "follow_up_delay": c.FollowUpDelay.String(), "answer_ttl": c.AnswerTTL.String(),
		"free_deals": c.FreeDeals, "share_buyer_contact": c.ShareBuyerContact}})
}

// List handles GET /api/deals?status=&limit=&offset= (newest first).
func (h *DealHandlers) List(w http.ResponseWriter, r *http.Request) {
	limit, offset, _, err := pageParams(r)
	if err != nil {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", err.Error())
		return
	}
	status := r.URL.Query().Get("status")
	if !dealStatuses[status] {
		writeErrorCode(w, http.StatusBadRequest, "BAD_REQUEST", "unknown status")
		return
	}
	rows, total, err := h.deals.List(r.Context(), status, limit, offset)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "listing deals", "error", err)
		writeErrorCode(w, http.StatusInternalServerError, "INTERNAL", "could not read deals")
		return
	}
	if rows == nil {
		rows = []*deal.Deal{}
	}
	w.Header().Set("X-Total-Count", strconv.Itoa(total))
	writeJSON(w, http.StatusOK, rows)
}
