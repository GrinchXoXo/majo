package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
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

	_, err = r.db.ExecContext(ctx, `
		INSERT INTO messages (id, conversation_id, direction, external_message_id, message_type, content, metadata, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (conversation_id, external_message_id) DO NOTHING
	`, msg.ID.String(), msg.ConversationID.String(), msg.Direction, externalID, msg.MessageType, contentBytes, metaBytes, msg.CreatedAt)
	if err != nil {
		return nil, err
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
