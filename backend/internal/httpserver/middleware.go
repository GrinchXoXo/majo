package httpserver

import (
	"net/http"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/logging"
)

// RequestID middleware ensures every request carries a request ID, generating
// one if the caller didn't supply it, and attaches it to the request context
// so all downstream logging includes it (Phase 0 requirement: PRD 0 FINAL §30).
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqID := r.Header.Get("X-Request-ID")
		if reqID == "" {
			reqID = uuid.NewString()
		}
		w.Header().Set("X-Request-ID", reqID)

		ctx := logging.WithRequestID(r.Context(), reqID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
