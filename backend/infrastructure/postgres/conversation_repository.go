package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/conversation"
)

// ConversationRepository is the database/sql-backed implementation of
// conversation.Repository. It is the only place in the codebase that
// should know about SQL for this domain.
type ConversationRepository struct {
	db *sql.DB
}

func NewConversationRepository(db *sql.DB) *ConversationRepository {
	return &ConversationRepository{db: db}
}

func (r *ConversationRepository) FindOrCreateByExternalID(ctx context.Context, channel, externalID string) (*conversation.Conversation, error) {
	var (
		c         conversation.Conversation
		metaBytes []byte
		buyerID   sql.NullString
		idStr     string
	)

	err := r.db.QueryRowContext(ctx, `
		INSERT INTO conversations (channel, external_id, status)
		VALUES ($1, $2, 'ACTIVE')
		ON CONFLICT (channel, external_id) DO UPDATE SET updated_at = now()
		RETURNING id, channel, external_id, buyer_id, status, metadata, created_at, updated_at
	`, channel, externalID).Scan(
		&idStr, &c.Channel, &c.ExternalID, &buyerID, &c.Status, &metaBytes, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	c.ID, err = uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}

	if buyerID.Valid {
		bid, err := uuid.Parse(buyerID.String)
		if err == nil {
			c.BuyerID = &bid
		}
	}

	if err := json.Unmarshal(metaBytes, &c.Metadata); err != nil {
		c.Metadata = map[string]any{}
	}

	return &c, nil
}

func (r *ConversationRepository) SaveMessage(ctx context.Context, msg *conversation.Message) (*conversation.Message, error) {
	contentBytes, err := json.Marshal(msg.Content)
	if err != nil {
		return nil, err
	}
	metaBytes, err := json.Marshal(msg.Metadata)
	if err != nil {
		return nil, err
	}

	if msg.ID == uuid.Nil {
		msg.ID = uuid.New()
	}
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now().UTC()
	}

	var externalID any
	if msg.ExternalMessageID != "" {
		externalID = msg.ExternalMessageID
	}

	res, err := r.db.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, direction, external_message_id, message_type, content, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (conversation_id, external_message_id) DO NOTHING
	`, msg.ID.String(), msg.ConversationID.String(), msg.Direction, externalID, msg.MessageType, contentBytes, metaBytes, msg.CreatedAt)
	if err != nil {
		return nil, err
	}
	// Before 3.1 a conflicting insert was silently reported as success, so
	// two concurrent deliveries of the same webhook could both be processed.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return nil, conversation.ErrDuplicateMessage
	}

	return msg, nil
}

func (r *ConversationRepository) LinkBuyer(ctx context.Context, conversationID, buyerID uuid.UUID) error {
	_, err := r.db.ExecContext(ctx, `
		UPDATE conversations SET buyer_id = $1, updated_at = now() WHERE id = $2
	`, buyerID.String(), conversationID.String())
	return err
}

func (r *ConversationRepository) FindByID(ctx context.Context, id uuid.UUID) (*conversation.Conversation, error) {
	var (
		c         conversation.Conversation
		idStr     string
		metaBytes []byte
		buyerID   sql.NullString
	)

	err := r.db.QueryRowContext(ctx, `
		SELECT id, channel, external_id, buyer_id, status, metadata, created_at, updated_at
		FROM conversations WHERE id = $1
	`, id.String()).Scan(
		&idStr, &c.Channel, &c.ExternalID, &buyerID, &c.Status, &metaBytes, &c.CreatedAt, &c.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	c.ID, err = uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	if buyerID.Valid {
		if bid, err := uuid.Parse(buyerID.String); err == nil {
			c.BuyerID = &bid
		}
	}
	if err := json.Unmarshal(metaBytes, &c.Metadata); err != nil {
		c.Metadata = map[string]any{}
	}

	return &c, nil
}

func (r *ConversationRepository) MessageExists(ctx context.Context, conversationID uuid.UUID, externalMessageID string) (bool, error) {
	if externalMessageID == "" {
		return false, nil
	}

	var exists bool
	err := r.db.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM messages
			WHERE conversation_id = $1 AND external_message_id = $2
		)
	`, conversationID.String(), externalMessageID).Scan(&exists)

	return exists, err
}

// ApplyDeliveryStatus implements conversation.DeliveryRecorder (§23A). Row
// lock + COALESCE make it idempotent: repeated or out-of-order receipts
// never move a timestamp, and First* flags are set only on first arrival.
func (r *ConversationRepository) ApplyDeliveryStatus(ctx context.Context, externalMessageID, status string, at time.Time, errDetail string) (*conversation.DeliveryTransition, error) {
	if externalMessageID == "" {
		return nil, nil
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var (
		idStr, convStr          string
		metaBytes               []byte
		delivered, read, failed sql.NullTime
	)
	err = tx.QueryRowContext(ctx, `
		SELECT id, conversation_id, metadata, delivered_at, read_at, failed_at
		FROM messages
		WHERE external_message_id = $1 AND direction = 'OUTBOUND'
		ORDER BY created_at DESC LIMIT 1
		FOR UPDATE
	`, externalMessageID).Scan(&idStr, &convStr, &metaBytes, &delivered, &read, &failed)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	t := &conversation.DeliveryTransition{}
	switch status {
	case conversation.DeliveryDelivered:
		t.FirstDelivered = !delivered.Valid
	case conversation.DeliveryRead:
		t.FirstDelivered = !delivered.Valid // read implies delivered
		t.FirstRead = !read.Valid
	case conversation.DeliveryFailed:
		t.FirstFailed = !failed.Valid
	default:
		return nil, nil // "sent" and unknown statuses carry no new fact
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE messages SET
			delivered_at   = CASE WHEN $2 IN ('delivered','read') THEN COALESCE(delivered_at, $3) ELSE delivered_at END,
			read_at        = CASE WHEN $2 = 'read'   THEN COALESCE(read_at, $3)   ELSE read_at END,
			failed_at      = CASE WHEN $2 = 'failed' THEN COALESCE(failed_at, $3) ELSE failed_at END,
			delivery_error = CASE WHEN $2 = 'failed' THEN NULLIF($4, '') ELSE delivery_error END
		WHERE id = $1
	`, idStr, status, at, errDetail); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE messages SET delivery_status = CASE
			WHEN read_at IS NOT NULL THEN 'read'
			WHEN failed_at IS NOT NULL THEN 'failed'
			WHEN delivered_at IS NOT NULL THEN 'delivered'
			ELSE delivery_status END
		WHERE id = $1
	`, idStr); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	msg := &conversation.Message{ExternalMessageID: externalMessageID, Direction: conversation.DirectionOutbound}
	msg.ID, _ = uuid.Parse(idStr)
	msg.ConversationID, _ = uuid.Parse(convStr)
	if err := json.Unmarshal(metaBytes, &msg.Metadata); err != nil || msg.Metadata == nil {
		msg.Metadata = map[string]any{}
	}
	t.Message = msg
	return t, nil
}

// OutboundEntityByProviderID implements routing.QuotedLookup (Phase 4C
// correction): the business entity an outbound message was about, from its
// provider message id (recorded by the recording sender).
func (r *ConversationRepository) OutboundEntityByProviderID(ctx context.Context, providerMessageID string) (string, *uuid.UUID, error) {
	var et, eid sql.NullString
	err := r.db.QueryRowContext(ctx, `SELECT metadata->>'entity_type', metadata->>'entity_id' FROM messages
		WHERE direction = 'OUTBOUND' AND external_message_id = $1 ORDER BY created_at DESC LIMIT 1`, providerMessageID).Scan(&et, &eid)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil, nil
	}
	if err != nil {
		return "", nil, err
	}
	id, perr := uuid.Parse(eid.String)
	if perr != nil {
		return et.String, nil, nil
	}
	return et.String, &id, nil
}
