package httpserver

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// OperatorAuth guards privileged operator actions (Phase 4A Step 4:
// listing edit, publish, reject, and listing media previews).
//
// Majo has no operator identity system yet (PRD 1 §42 deferred it), so this
// is deliberately the smallest boundary that keeps publication from being
// publicly callable: one shared secret, sent as "Authorization: Bearer
// <token>", compared in constant time. It identifies "an operator", not
// which operator, so audit entries carry no operator ID.
//
// token == "" is only allowed in development (config enforces this); then
// requests pass through unauthenticated, exactly as every other operator
// endpoint does today.
type OperatorAuth struct {
	tokenHash [32]byte
	open      bool
}

func NewOperatorAuth(token string) *OperatorAuth {
	if token == "" {
		return &OperatorAuth{open: true}
	}
	return &OperatorAuth{tokenHash: sha256.Sum256([]byte(token))}
}

// Open reports whether the guard is disabled (development without a token).
func (a *OperatorAuth) Open() bool { return a.open }

// Authorized reports whether the request carries the operator token.
func (a *OperatorAuth) Authorized(r *http.Request) bool {
	if a.open {
		return true
	}
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return false
	}
	got := sha256.Sum256([]byte(strings.TrimSpace(h[len(prefix):])))
	return subtle.ConstantTimeCompare(got[:], a.tokenHash[:]) == 1
}

// Require wraps a handler so it only runs for authorized operators.
func (a *OperatorAuth) Require(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !a.Authorized(r) {
			w.Header().Set("WWW-Authenticate", `Bearer realm="majo-operator"`)
			writeErrorCode(w, http.StatusUnauthorized, "UNAUTHORIZED", "operator authorization required")
			return
		}
		next(w, r)
	}
}

// writeErrorCode is writeError plus a stable machine-readable code, so the
// dashboard can react to specific failures (stale edit, invalid transition)
// without parsing messages. The existing {"error": "..."} shape is kept.
func writeErrorCode(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}
