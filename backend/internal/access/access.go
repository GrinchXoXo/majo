// Package access defines operator principals and roles for the control plane
// (reuse-audit R1). The HTTP layer authenticates; repositories resolve named
// operator tokens.
package access

import (
	"context"

	"github.com/google/uuid"
)

// Role of an operator principal.
type Role string

const (
	// RoleViewer may call read routes only.
	RoleViewer Role = "VIEWER"
	// RoleOperator may call every control-plane route.
	RoleOperator Role = "OPERATOR"
)

// Principal is the authenticated caller of a control-plane route.
type Principal struct {
	ID   *uuid.UUID // nil for the shared token and open development mode
	Name string
	Role Role
}

// Can reports whether the principal's role satisfies need.
func (p Principal) Can(need Role) bool {
	return p.Role == RoleOperator || (need == RoleViewer && p.Role == RoleViewer)
}

// OperatorStore resolves named operator tokens.
type OperatorStore interface {
	// FindActiveByTokenHash returns the active operator whose token hashes to
	// h, or ok=false.
	FindActiveByTokenHash(ctx context.Context, h [32]byte) (p Principal, ok bool, err error)
}
