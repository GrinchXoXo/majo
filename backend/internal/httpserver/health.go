package httpserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"

	"github.com/middleman/backend/infrastructure/ai"
)

type healthResponse struct {
	Status string            `json:"status"`
	Checks map[string]string `json:"checks,omitempty"`
}

// HealthHandler responds to GET /health with a simple liveness check.
func HealthHandler(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, healthResponse{Status: "ok"})
}

// ReadyHandler responds to GET /ready, verifying that required dependencies
// (Postgres, the AI service) are actually reachable — not just that the Go
// process itself is running.
func ReadyHandler(db *sql.DB, aiProvider ai.Provider) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()

		checks := map[string]string{}
		ok := true

		if err := db.PingContext(ctx); err != nil {
			checks["postgres"] = "unreachable: " + err.Error()
			ok = false
		} else {
			checks["postgres"] = "ok"
		}

		if err := aiProvider.Healthy(ctx); err != nil {
			checks["ai_service"] = "unreachable: " + err.Error()
			ok = false
		} else {
			checks["ai_service"] = "ok"
		}

		status := http.StatusOK
		resp := healthResponse{Status: "ok", Checks: checks}
		if !ok {
			status = http.StatusServiceUnavailable
			resp.Status = "degraded"
		}

		writeJSON(w, status, resp)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
