// Package buyer owns buyer identity linked to a conversation.
// Per PRD 1 §7: this is intentionally minimal — not a full customer
// account system. The external user identifier comes from the channel
// (WhatsApp wa_id for now).
package buyer

import (
	"context"
	"time"

	"github.com/google/uuid"
)

type Buyer struct {
	ID             uuid.UUID
	Channel        string
	ExternalUserID string
	DisplayName    string
	Metadata       map[string]any
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// Repository is the persistence boundary for buyers.
type Repository interface {
	FindOrCreateByExternalID(ctx context.Context, channel, externalUserID string) (*Buyer, error)
}
