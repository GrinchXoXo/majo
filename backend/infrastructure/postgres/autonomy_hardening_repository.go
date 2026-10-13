package postgres

import (
	"context"
	"database/sql"
	"errors"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/autonomy"
)

// Phase 4C hardening: queued buyer notices and operator holds.

var (
	_ autonomy.NoticeStore = (*AutonomyRepository)(nil)
	_ autonomy.HoldStore   = (*AutonomyRepository)(nil)
)

const noticeColumns = `id, conversation_id, buyer_request_id, kind, body, status, COALESCE(failure_class, ''), COALESCE(failure_code, 0),
	attempts, COALESCE(last_error, ''), source_message_id, created_at, sent_at`

func scanNotice(sc interface{ Scan(...any) error }) (*autonomy.Notice, error) {
	var (
		n        autonomy.Notice
		id, conv string
		br, src  sql.NullString
		sent     sql.NullTime
	)
	if err := sc.Scan(&id, &conv, &br, &n.Kind, &n.Body, &n.Status, &n.FailureClass, &n.FailureCode, &n.Attempts, &n.LastError,
		&src, &n.CreatedAt, &sent); err != nil {
		return nil, err
	}
	n.ID, _ = uuid.Parse(id)
	n.ConversationID, _ = uuid.Parse(conv)
	n.BuyerRequestID, n.SourceMessageID = optUUID(br), optUUID(src)
	n.SentAt = optTime(sent)
	return &n, nil
}

// QueueNotice implements autonomy.NoticeStore. The partial unique indexes
// make a duplicate (same failed message, or a second pending offer
// re-delivery for the request) a no-op that returns the existing notice.
func (r *AutonomyRepository) QueueNotice(ctx context.Context, n *autonomy.Notice) (*autonomy.Notice, bool, error) {
	if n.ID == uuid.Nil {
		n.ID = uuid.New()
	}
	var code any
	if n.FailureCode != 0 {
		code = n.FailureCode
	}
	res, err := r.db.ExecContext(ctx, `INSERT INTO pending_notices
		(id, conversation_id, buyer_request_id, kind, body, status, failure_class, failure_code, attempts, last_error, source_message_id)
		VALUES ($1, $2, $3, $4, $5, 'PENDING', $6, $7, $8, $9, $10) ON CONFLICT DO NOTHING`,
		n.ID.String(), n.ConversationID.String(), uuidArg(n.BuyerRequestID), n.Kind, n.Body, nullIfEmpty(n.FailureClass), code,
		n.Attempts, nullIfEmpty(n.LastError), uuidArg(n.SourceMessageID))
	if err != nil {
		return nil, false, err
	}
	if k, _ := res.RowsAffected(); k == 1 {
		got, err := r.FindNotice(ctx, n.ID)
		return got, true, err
	}
	// Duplicate: return the existing one.
	var row *sql.Row
	if n.SourceMessageID != nil {
		row = r.db.QueryRowContext(ctx, `SELECT `+noticeColumns+` FROM pending_notices WHERE source_message_id = $1`, n.SourceMessageID.String())
	} else {
		row = r.db.QueryRowContext(ctx, `SELECT `+noticeColumns+` FROM pending_notices
			WHERE buyer_request_id = $1 AND kind = $2 AND status = 'PENDING' ORDER BY created_at DESC LIMIT 1`, uuidArg(n.BuyerRequestID), n.Kind)
	}
	existing, err := scanNotice(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return existing, false, err
}

// PendingNotices implements autonomy.NoticeStore (oldest first).
func (r *AutonomyRepository) PendingNotices(ctx context.Context, conversationID uuid.UUID) ([]*autonomy.Notice, error) {
	return r.notices(ctx, `SELECT `+noticeColumns+` FROM pending_notices WHERE conversation_id = $1 AND status = 'PENDING' ORDER BY created_at, id`,
		conversationID.String())
}

// NoticesForRequest implements autonomy.NoticeStore.
func (r *AutonomyRepository) NoticesForRequest(ctx context.Context, buyerRequestID uuid.UUID) ([]*autonomy.Notice, error) {
	return r.notices(ctx, `SELECT `+noticeColumns+` FROM pending_notices WHERE buyer_request_id = $1 ORDER BY created_at, id`, buyerRequestID.String())
}

func (r *AutonomyRepository) notices(ctx context.Context, q string, args ...any) ([]*autonomy.Notice, error) {
	rows, err := r.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*autonomy.Notice{}
	for rows.Next() {
		n, err := scanNotice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// FindNotice implements autonomy.NoticeStore.
func (r *AutonomyRepository) FindNotice(ctx context.Context, id uuid.UUID) (*autonomy.Notice, error) {
	n, err := scanNotice(r.db.QueryRowContext(ctx, `SELECT `+noticeColumns+` FROM pending_notices WHERE id = $1`, id.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return n, err
}

// MarkNotice implements autonomy.NoticeStore: only a PENDING notice moves.
func (r *AutonomyRepository) MarkNotice(ctx context.Context, id uuid.UUID, status, lastErr string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE pending_notices SET status = $2, attempts = attempts + 1,
		last_error = COALESCE($3, last_error), updated_at = now(),
		sent_at = CASE WHEN $2 = 'SENT' THEN now() ELSE sent_at END
		WHERE id = $1 AND status = 'PENDING'`, id.String(), status, nullIfEmpty(lastErr))
	if err != nil {
		return false, err
	}
	k, _ := res.RowsAffected()
	return k == 1, nil
}

const holdColumns = `id, buyer_request_id, held_by, reason, held_at, COALESCE(released_by, ''), COALESCE(release_reason, ''), released_at`

func scanHold(sc interface{ Scan(...any) error }) (*autonomy.Hold, error) {
	var (
		h        autonomy.Hold
		id, br   string
		released sql.NullTime
	)
	if err := sc.Scan(&id, &br, &h.HeldBy, &h.Reason, &h.HeldAt, &h.ReleasedBy, &h.ReleaseReason, &released); err != nil {
		return nil, err
	}
	h.ID, _ = uuid.Parse(id)
	h.BuyerRequestID, _ = uuid.Parse(br)
	h.ReleasedAt = optTime(released)
	return &h, nil
}

// PlaceHold implements autonomy.HoldStore.
func (r *AutonomyRepository) PlaceHold(ctx context.Context, buyerRequestID uuid.UUID, by, reason string) (*autonomy.Hold, bool, error) {
	res, err := r.db.ExecContext(ctx, `INSERT INTO workflow_holds (buyer_request_id, held_by, reason) VALUES ($1, $2, $3) ON CONFLICT DO NOTHING`,
		buyerRequestID.String(), by, reason)
	if err != nil {
		return nil, false, err
	}
	k, _ := res.RowsAffected()
	h, err := r.ActiveHold(ctx, buyerRequestID)
	return h, k == 1, err
}

// ReleaseHold implements autonomy.HoldStore.
func (r *AutonomyRepository) ReleaseHold(ctx context.Context, buyerRequestID uuid.UUID, by, reason string) (bool, error) {
	res, err := r.db.ExecContext(ctx, `UPDATE workflow_holds SET released_by = $2, release_reason = $3, released_at = now()
		WHERE buyer_request_id = $1 AND released_at IS NULL`, buyerRequestID.String(), by, reason)
	if err != nil {
		return false, err
	}
	k, _ := res.RowsAffected()
	return k == 1, nil
}

// ActiveHold implements autonomy.HoldStore.
func (r *AutonomyRepository) ActiveHold(ctx context.Context, buyerRequestID uuid.UUID) (*autonomy.Hold, error) {
	h, err := scanHold(r.db.QueryRowContext(ctx, `SELECT `+holdColumns+` FROM workflow_holds WHERE buyer_request_id = $1 AND released_at IS NULL`,
		buyerRequestID.String()))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return h, err
}

// Holds implements autonomy.HoldStore (newest first).
func (r *AutonomyRepository) Holds(ctx context.Context, buyerRequestID uuid.UUID) ([]*autonomy.Hold, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT `+holdColumns+` FROM workflow_holds WHERE buyer_request_id = $1 ORDER BY held_at DESC`, buyerRequestID.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*autonomy.Hold{}
	for rows.Next() {
		h, err := scanHold(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}
