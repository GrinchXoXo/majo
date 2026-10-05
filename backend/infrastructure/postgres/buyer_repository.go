package postgres

import (
	"context"
	"database/sql"
	"encoding/json"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/buyer"
)

type BuyerRepository struct {
	db *sql.DB
}

func NewBuyerRepository(db *sql.DB) *BuyerRepository {
	return &BuyerRepository{db: db}
}

func (r *BuyerRepository) FindOrCreateByExternalID(ctx context.Context, channel, externalUserID string) (*buyer.Buyer, error) {
	var (
		b         buyer.Buyer
		idStr     string
		metaBytes []byte
		name      sql.NullString
	)

	err := r.db.QueryRowContext(ctx, `
		INSERT INTO buyers (channel, external_user_id)
		VALUES ($1, $2)
		ON CONFLICT (channel, external_user_id) DO UPDATE SET updated_at = now()
		RETURNING id, channel, external_user_id, display_name, metadata, created_at, updated_at
	`, channel, externalUserID).Scan(
		&idStr, &b.Channel, &b.ExternalUserID, &name, &metaBytes, &b.CreatedAt, &b.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}

	b.ID, err = uuid.Parse(idStr)
	if err != nil {
		return nil, err
	}
	if name.Valid {
		b.DisplayName = name.String
	}
	if err := json.Unmarshal(metaBytes, &b.Metadata); err != nil {
		b.Metadata = map[string]any{}
	}

	return &b, nil
}
