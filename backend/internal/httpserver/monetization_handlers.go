package httpserver

// Pilot batch item 8: read-only view of the monetization scaffold. Shows
// the stage, the payment provider and every revenue line with its gate, so
// the dashboard can say plainly that nothing is charged.

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/middleman/backend/internal/monetization"
)

// LedgerSummary is implemented by postgres.LedgerRepository.
type LedgerSummary interface {
	Summary(ctx context.Context) (entries int, balances map[string]int64, err error)
}

type MonetizationHandlers struct {
	settings monetization.Settings
	ledger   LedgerSummary
	logger   *slog.Logger
}

func NewMonetizationHandlers(s monetization.Settings, l LedgerSummary, logger *slog.Logger) *MonetizationHandlers {
	return &MonetizationHandlers{settings: s, ledger: l, logger: logger}
}

type revenueLineView struct {
	Line        monetization.Line        `json:"line"`
	On          bool                     `json:"on"`
	EnvVar      string                   `json:"env_var"`
	Requirement monetization.Requirement `json:"requirement"`
}

// Get handles GET /api/monetization.
func (h *MonetizationHandlers) Get(w http.ResponseWriter, r *http.Request) {
	lines := make([]revenueLineView, 0, len(monetization.Lines))
	for _, l := range monetization.Lines {
		lines = append(lines, revenueLineView{Line: l, On: h.settings.On(l), EnvVar: monetization.EnvName(l), Requirement: monetization.Requirements[l]})
	}
	out := map[string]any{"stage": h.settings.Stage, "provider": h.settings.Provider, "lines": lines,
		"customer_funds_allowed": h.settings.CustomerFundsAllowed()}
	if h.ledger != nil {
		n, balances, err := h.ledger.Summary(r.Context())
		if err != nil {
			h.logger.ErrorContext(r.Context(), "ledger summary", "error", err)
			writeErrorCode(w, http.StatusInternalServerError, "INTERNAL", "could not read the ledger")
			return
		}
		out["ledger"] = map[string]any{"entries": n, "balances_kobo": balances}
	}
	writeJSON(w, http.StatusOK, out)
}
