package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/event"
)

// EventWriter is the database/sql-backed implementation of event.Writer.
type EventWriter struct {
	db *sql.DB
}

func NewEventWriter(db *sql.DB) *EventWriter {
	return &EventWriter{db: db}
}

func (w *EventWriter) Write(ctx context.Context, e *event.Event) error {
	if e.ID == uuid.Nil {
		e.ID = uuid.New()
	}
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	if e.Payload == nil {
		e.Payload = map[string]any{}
	}

	payloadBytes, err := json.Marshal(e.Payload)
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
		INSERT INTO events (id, event_type, entity_type, entity_id, actor_type, actor_id, payload, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, e.ID.String(), e.EventType, e.EntityType, entityID, e.ActorType, actorID, payloadBytes, e.CreatedAt)

	return err
}

// ListByEntity implements event.Reader.
func (w *EventWriter) ListByEntity(ctx context.Context, entityType string, entityID uuid.UUID) ([]*event.Event, error) {
	rows, err := w.db.QueryContext(ctx, `
		SELECT id, event_type, entity_type, entity_id, actor_type, actor_id, payload, created_at
		FROM events
		WHERE entity_type = $1 AND entity_id = $2
		ORDER BY created_at ASC
	`, entityType, entityID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var results []*event.Event
	for rows.Next() {
		var (
			e                    event.Event
			idStr                string
			entityIDStr          sql.NullString
			actorIDStr           sql.NullString
			payloadBytes         []byte
		)
		if err := rows.Scan(&idStr, &e.EventType, &e.EntityType, &entityIDStr, &e.ActorType, &actorIDStr, &payloadBytes, &e.CreatedAt); err != nil {
			return nil, err
		}
		e.ID, _ = uuid.Parse(idStr)
		if entityIDStr.Valid {
			if eid, err := uuid.Parse(entityIDStr.String); err == nil {
				e.EntityID = &eid
			}
		}
		if actorIDStr.Valid {
			if aid, err := uuid.Parse(actorIDStr.String); err == nil {
				e.ActorID = &aid
			}
		}
		_ = json.Unmarshal(payloadBytes, &e.Payload)
		if e.Payload == nil {
			e.Payload = map[string]any{}
		}
		results = append(results, &e)
	}
	return results, rows.Err()
}
