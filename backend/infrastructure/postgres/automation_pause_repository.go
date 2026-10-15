package postgres

// Pilot batch item 4: the global automation pause (migration 0017).

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/autonomy"
)

const pauseColumns = `id, paused_by, reason, paused_at, released_by, release_reason, released_at`

func scanPause(row interface{ Scan(...any) error }) (*autonomy.AutomationPause, error) {
	var p autonomy.AutomationPause
	var id string
	var relBy, relReason sql.NullString
	var relAt sql.NullTime
	if err := row.Scan(&id, &p.PausedBy, &p.Reason, &p.PausedAt, &relBy, &relReason, &relAt); err != nil {
		return nil, err
	}
	p.ID, _ = uuid.Parse(id)
	p.PausedAt = p.PausedAt.UTC()
	if relBy.Valid {
		p.ReleasedBy = &relBy.String
	}
	if relReason.Valid {
		p.ReleaseReason = &relReason.String
	}
	if relAt.Valid {
		t := relAt.Time.UTC()
		p.ReleasedAt = &t
	}
	return &p, nil
}

// ActivePause implements autonomy.PauseStore.
func (r *AutonomyRepository) ActivePause(ctx context.Context) (*autonomy.AutomationPause, error) {
	p, err := scanPause(r.db.QueryRowContext(ctx, `SELECT `+pauseColumns+` FROM automation_pauses WHERE scope = 'global' AND released_at IS NULL`))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return p, err
}

// PlacePause implements autonomy.PauseStore (idempotent: one active pause).
func (r *AutonomyRepository) PlacePause(ctx context.Context, by, reason string) (*autonomy.AutomationPause, bool, error) {
	res, err := r.db.ExecContext(ctx, `INSERT INTO automation_pauses (paused_by, reason) VALUES ($1, $2) ON CONFLICT DO NOTHING`, by, reason)
	if err != nil {
		return nil, false, err
	}
	k, _ := res.RowsAffected()
	p, err := r.ActivePause(ctx)
	return p, k == 1, err
}

// ReleasePause implements autonomy.PauseStore.
func (r *AutonomyRepository) ReleasePause(ctx context.Context, by, reason string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE automation_pauses SET released_by = $1, release_reason = $2, released_at = now()
		WHERE scope = 'global' AND released_at IS NULL`, by, reason)
	if err != nil {
		return false, err
	}
	k, _ := res.RowsAffected()
	return k == 1, nil
}

// RecentPauses implements autonomy.PauseStore.
func (r *AutonomyRepository) RecentPauses(ctx context.Context, limit int) ([]*autonomy.AutomationPause, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+pauseColumns+` FROM automation_pauses ORDER BY paused_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*autonomy.AutomationPause
	for rows.Next() {
		p, err := scanPause(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// OpenConfirmedRequests implements autonomy.PauseStore.
func (r *AutonomyRepository) OpenConfirmedRequests(ctx context.Context, since time.Time) ([]uuid.UUID, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT br.id FROM buyer_requests br
		WHERE br.status = 'CONFIRMED' AND br.created_at >= $1
		AND NOT EXISTS (SELECT 1 FROM buyer_selections s WHERE s.buyer_request_id = br.id AND s.status = 'ACTIVE')
		ORDER BY br.created_at`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if u, err := uuid.Parse(id); err == nil {
			out = append(out, u)
		}
	}
	return out, rows.Err()
}
