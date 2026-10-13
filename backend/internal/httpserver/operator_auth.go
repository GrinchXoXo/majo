package httpserver

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/access"
)

// OperatorAuth guards the operator control plane: every /api route.
//
// Two kinds of credential are accepted, both sent as
// "Authorization: Bearer <token>" and compared by SHA-256 hash:
//
//   - Named operator tokens (operators table, managed with cmd/operatorctl).
//     Each identifies one person and carries a role, and the person's name and
//     ID are recorded as the actor of what they do.
//   - The shared OPERATOR_API_TOKEN (Phase 4A Step 4). It is kept as the
//     bootstrap / break-glass credential: role OPERATOR, actor name "operator",
//     no operator ID.
//
// Roles: VIEWER may call read routes (GET); OPERATOR may call every route.
//
// With no shared token and no active named operator, the guard is open; config
// only allows that with APP_ENV=development set explicitly.
type OperatorAuth struct {
	tokenHash [32]byte
	hasToken  bool
	store     OperatorStore
	open      bool
	logger    *slog.Logger
}

// Aliases of the access package's types, so handlers read naturally.
type (
	Role          = access.Role
	Principal     = access.Principal
	OperatorStore = access.OperatorStore
)

const (
	RoleViewer   = access.RoleViewer
	RoleOperator = access.RoleOperator
)

func NewOperatorAuth(token string) *OperatorAuth {
	if token == "" {
		return &OperatorAuth{open: true}
	}
	return &OperatorAuth{tokenHash: sha256.Sum256([]byte(token)), hasToken: true}
}

// SetStore enables named operator tokens. activeOperators is the number of
// active named operators at startup: if any exist, the guard is never open,
// even in development without a shared token.
func (a *OperatorAuth) SetStore(s OperatorStore, activeOperators int, logger *slog.Logger) {
	a.store = s
	a.logger = logger
	if activeOperators > 0 {
		a.open = false
	}
}

// Open reports whether the guard is disabled (development without credentials).
func (a *OperatorAuth) Open() bool { return a.open }

// Authenticate resolves the request's principal.
func (a *OperatorAuth) Authenticate(r *http.Request) (Principal, bool) {
	if a.open {
		return Principal{Name: "operator", Role: RoleOperator}, true
	}
	h := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return Principal{}, false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return Principal{}, false
	}
	got := sha256.Sum256([]byte(tok))
	if a.hasToken && subtle.ConstantTimeCompare(got[:], a.tokenHash[:]) == 1 {
		return Principal{Name: "operator", Role: RoleOperator}, true
	}
	if a.store != nil {
		p, ok, err := a.store.FindActiveByTokenHash(r.Context(), got)
		if err != nil {
			if a.logger != nil {
				a.logger.Error("operator lookup failed", "error", err)
			}
			return Principal{}, false
		}
		if ok {
			return p, true
		}
	}
	return Principal{}, false
}

// Authorized reports whether the request carries an OPERATOR-role credential.
func (a *OperatorAuth) Authorized(r *http.Request) bool {
	p, ok := a.Authenticate(r)
	return ok && p.Can(RoleOperator)
}

// Require wraps a handler so it only runs for OPERATOR-role principals.
func (a *OperatorAuth) Require(next http.HandlerFunc) http.HandlerFunc {
	return a.require(RoleOperator, next)
}

// RequireViewer wraps a read handler: VIEWER or OPERATOR.
func (a *OperatorAuth) RequireViewer(next http.HandlerFunc) http.HandlerFunc {
	return a.require(RoleViewer, next)
}

func (a *OperatorAuth) require(need Role, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := a.Authenticate(r)
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="majo-operator"`)
			writeErrorCode(w, http.StatusUnauthorized, "UNAUTHORIZED", "operator authorization required")
			return
		}
		if !p.Can(need) {
			writeErrorCode(w, http.StatusForbidden, "FORBIDDEN", "this operator's role cannot perform this action")
			return
		}
		next(w, r.WithContext(WithPrincipal(r.Context(), p)))
	}
}

type principalKey struct{}

// WithPrincipal attaches the authenticated principal to ctx.
func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal attached by Require/RequireViewer. A
// handler reached without the guard (tests) gets the anonymous "operator".
func PrincipalFrom(ctx context.Context) Principal {
	if p, ok := ctx.Value(principalKey{}).(Principal); ok {
		return p
	}
	return Principal{Name: "operator", Role: RoleOperator}
}

// actorName is the name recorded for the caller in holds, resolutions and
// batches ("operator" for the shared token).
func actorName(r *http.Request) string { return PrincipalFrom(r.Context()).Name }

// actorID is the caller's operator ID for audit/event rows (nil for the shared
// token).
func actorID(r *http.Request) *uuid.UUID { return PrincipalFrom(r.Context()).ID }

// writeErrorCode is writeError plus a stable machine-readable code, so the
// dashboard can react to specific failures (stale edit, invalid transition)
// without parsing messages. The existing {"error": "..."} shape is kept.
func writeErrorCode(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]string{"error": message, "code": code})
}
