package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/audit"
)

// AuditWriter is the database/sql-backed implementation of audit.Writer.
type AuditWriter struct {
	db *sql.DB
}

func NewAuditWriter(db *sql.DB) *AuditWriter {
	return &AuditWriter{db: db}
}

func (w *AuditWriter) Write(ctx context.Context, e *audit.Entry) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.Metadata == nil {
		e.Metadata = map[string]any{}
	}

	// Use `any` (not []byte) so an unset value passes through to the SQL
	// driver as a true NULL. A nil-valued []byte variable is NOT the same
	// as an untyped nil interface: passing the former through database/sql
	// to a jsonb column causes `pq: invalid input syntax for type json`
	// (confirmed via reproduction during the PRD 2.1 audit) because the
	// driver sees a concrete-but-empty []byte rather than SQL NULL.
	var prevBytes, newBytes any
	if e.PreviousValue != nil {
		b, err := json.Marshal(e.PreviousValue)
		if err != nil {
			return err
		}
		prevBytes = b
	}
	if e.NewValue != nil {
		b, err := json.Marshal(e.NewValue)
		if err != nil {
			return err
		}
		newBytes = b
	}
	metaBytes, err := json.Marshal(e.Metadata)
	if err != nil {
		return err
	}

	var entityID, actorID any
	if e.EntityID != nil {
		entityID = e.EntityID.String()
	}
	if e.ActorID != nil {
		actorID = e.ActorID.String()
	}

	_, err = w.db.ExecContext(ctx, `
		INSERT INTO audit_logs (id, actor_type, actor_id, action, entity_type, entity_id, previous_value, new_value, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, e.ID.String(), e.ActorType, actorID, e.Action, e.EntityType, entityID, prevBytes, newBytes, metaBytes, e.CreatedAt)

	return err
}
