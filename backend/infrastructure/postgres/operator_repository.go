package postgres

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/access"
)

// OperatorRepository stores named operator credentials (migration 0016).
// Tokens are generated here, shown once, and stored only as SHA-256 hashes.
type OperatorRepository struct {
	db *sql.DB
}

func NewOperatorRepository(db *sql.DB) *OperatorRepository { return &OperatorRepository{db: db} }

// Operator is one named credential (never includes the token).
type Operator struct {
	ID         uuid.UUID
	Name       string
	Role       access.Role
	Active     bool
	CreatedAt  time.Time
	RevokedAt  *time.Time
	LastUsedAt *time.Time
}

// ErrOperatorNotFound is returned by Revoke for an unknown or already-revoked name.
var ErrOperatorNotFound = errors.New("operator not found or already revoked")

// FindActiveByTokenHash implements access.OperatorStore. last_used_at is
// refreshed at most once a minute, so authentication is not a write per request.
func (r *OperatorRepository) FindActiveByTokenHash(ctx context.Context, h [32]byte) (access.Principal, bool, error) {
	var id uuid.UUID
	var name, role string
	err := r.db.QueryRowContext(ctx, `
		UPDATE operators SET last_used_at = now()
		 WHERE token_hash = $1 AND active
		   AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')
		RETURNING id, name, role`, h[:]).Scan(&id, &name, &role)
	if errors.Is(err, sql.ErrNoRows) {
		err = r.db.QueryRowContext(ctx, `SELECT id, name, role FROM operators WHERE token_hash = $1 AND active`, h[:]).Scan(&id, &name, &role)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return access.Principal{}, false, nil
	}
	if err != nil {
		return access.Principal{}, false, err
	}
	return access.Principal{ID: &id, Name: name, Role: access.Role(role)}, true, nil
}

// CountActive returns the number of active named operators.
func (r *OperatorRepository) CountActive(ctx context.Context) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM operators WHERE active`).Scan(&n)
	return n, err
}

// Create adds a named operator and returns its token (shown once).
func (r *OperatorRepository) Create(ctx context.Context, name string, role access.Role) (Operator, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 80 {
		return Operator{}, "", fmt.Errorf("name must be 1-80 characters")
	}
	if role != access.RoleViewer && role != access.RoleOperator {
		return Operator{}, "", fmt.Errorf("role must be VIEWER or OPERATOR")
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return Operator{}, "", err
	}
	token := "majo_op_" + hex.EncodeToString(raw)
	h := sha256.Sum256([]byte(token))
	o := Operator{Name: name, Role: role, Active: true}
	err := r.db.QueryRowContext(ctx, `
		INSERT INTO operators (name, role, token_hash) VALUES ($1, $2, $3)
		RETURNING id, created_at`, name, string(role), h[:]).Scan(&o.ID, &o.CreatedAt)
	if err != nil {
		return Operator{}, "", err
	}
	return o, token, nil
}

// Revoke deactivates an operator by name.
func (r *OperatorRepository) Revoke(ctx context.Context, name string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE operators SET active = false, revoked_at = now() WHERE name = $1 AND active`, strings.TrimSpace(name))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrOperatorNotFound
	}
	return nil
}

// List returns every operator, active first.
func (r *OperatorRepository) List(ctx context.Context) ([]Operator, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, name, role, active, created_at, revoked_at, last_used_at
		  FROM operators ORDER BY active DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Operator
	for rows.Next() {
		var o Operator
		var role string
		if err := rows.Scan(&o.ID, &o.Name, &role, &o.Active, &o.CreatedAt, &o.RevokedAt, &o.LastUsedAt); err != nil {
			return nil, err
		}
		o.Role = access.Role(role)
		out = append(out, o)
	}
	return out, rows.Err()
}
