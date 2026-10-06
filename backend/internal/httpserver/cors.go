package httpserver

import "net/http"

// CORS applies a permissive policy so the Next.js dashboard (running on a
// different port during local development) can call the API directly from
// the browser. Phase 1 is an internal tool on a private network — this is
// deliberately simple rather than a configurable allow-list, per PRD 1 §42
// ("do not spend Phase 1 building a complete... identity system").
// Tighten this before any public-facing deployment.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}
