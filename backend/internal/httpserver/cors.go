package httpserver

import (
	"net/http"
	"strings"
)

// CORS lets only the configured dashboard origins call the API from a browser
// (CORS_ALLOWED_ORIGINS, default DASHBOARD_URL). Other origins get no
// Access-Control-Allow-Origin header, so browsers refuse to expose responses
// to them. Non-browser clients are unaffected; the operator token is what
// authorizes a call, CORS only stops other websites from using a logged-in
// operator's browser.
func CORS(allowed []string) func(http.Handler) http.Handler {
	set := map[string]bool{}
	for _, o := range allowed {
		if o = strings.TrimRight(strings.TrimSpace(o), "/"); o != "" {
			set[o] = true
		}
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			ok := origin != "" && set[origin]
			if origin != "" {
				w.Header().Add("Vary", "Origin")
			}
			if ok {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				w.Header().Set("Access-Control-Expose-Headers", "X-Total-Count, X-Request-ID")
			}
			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				if !ok {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
