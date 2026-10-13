package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/middleman/backend/internal/controlplane"
)

// ControlPlaneRepository serves the operator dashboard's read views over
// Majo's own records (reuse-audit R1): the event timeline and the durable job
// queue. Read-only; paginated with limit/offset and a total count.
type ControlPlaneRepository struct {
	db *sql.DB
}

func NewControlPlaneRepository(db *sql.DB) *ControlPlaneRepository {
	return &ControlPlaneRepository{db: db}
}

type where struct {
	parts []string
	args  []any
}

func (w *where) add(expr string, v any) {
	w.args = append(w.args, v)
	w.parts = append(w.parts, fmt.Sprintf(expr, len(w.args)))
}

func (w *where) sql() string {
	if len(w.parts) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.parts, " AND ")
}

// ListEvents returns events newest first, and the total matching count.
func (r *ControlPlaneRepository) ListEvents(ctx context.Context, f controlplane.EventFilter, limit, offset int) ([]controlplane.EventRow, int, error) {
	var w where
	if f.EventType != "" {
		w.add("e.event_type = $%d", f.EventType)
	}
	if f.EntityType != "" {
		w.add("e.entity_type = $%d", f.EntityType)
	}
	if f.EntityID != nil {
		w.add("e.entity_id = $%d", *f.EntityID)
	}
	if f.ActorType != "" {
		w.add("e.actor_type = $%d", f.ActorType)
	}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM events e`+w.sql(), w.args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args := append(append([]any{}, w.args...), limit, offset)
	rows, err := r.db.QueryContext(ctx, `
		SELECT e.id, e.event_type, e.entity_type, e.entity_id, e.actor_type, e.actor_id, o.name, e.payload, e.created_at
		  FROM events e LEFT JOIN operators o ON o.id = e.actor_id`+w.sql()+
		fmt.Sprintf(` ORDER BY e.created_at DESC, e.id DESC LIMIT $%d OFFSET $%d`, len(w.args)+1, len(w.args)+2), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []controlplane.EventRow{}
	for rows.Next() {
		var e controlplane.EventRow
		var payload []byte
		if err := rows.Scan(&e.ID, &e.EventType, &e.EntityType, &e.EntityID, &e.ActorType, &e.ActorID, &e.ActorName, &payload, &e.CreatedAt); err != nil {
			return nil, 0, err
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// EventTypes returns the distinct event types, for the timeline filter.
func (r *ControlPlaneRepository) EventTypes(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT event_type FROM events ORDER BY event_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListJobs returns jobs, most recently updated first.
func (r *ControlPlaneRepository) ListJobs(ctx context.Context, f controlplane.JobFilter, limit, offset int) ([]controlplane.JobRow, int, error) {
	var w where
	if f.Status != "" {
		w.add("status = $%d", f.Status)
	}
	if f.JobType != "" {
		w.add("job_type = $%d", f.JobType)
	}
	var total int
	if err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs`+w.sql(), w.args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args := append(append([]any{}, w.args...), limit, offset)
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, job_type, idempotency_key, status, attempts, max_attempts, run_at, locked_by, last_error, created_at, updated_at, completed_at
		  FROM jobs`+w.sql()+fmt.Sprintf(` ORDER BY updated_at DESC, id DESC LIMIT $%d OFFSET $%d`, len(w.args)+1, len(w.args)+2), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []controlplane.JobRow{}
	for rows.Next() {
		var j controlplane.JobRow
		if err := rows.Scan(&j.ID, &j.JobType, &j.IdempotencyKey, &j.Status, &j.Attempts, &j.MaxAttempts, &j.RunAt,
			&j.LockedBy, &j.LastError, &j.CreatedAt, &j.UpdatedAt, &j.CompletedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, j)
	}
	return out, total, rows.Err()
}

// JobSummary returns per-type counts.
func (r *ControlPlaneRepository) JobSummary(ctx context.Context) ([]controlplane.JobSummaryRow, error) {
	rows, err := r.db.QueryContext(ctx, `
		SELECT job_type,
		       count(*) FILTER (WHERE status = 'PENDING'),
		       count(*) FILTER (WHERE status = 'RUNNING'),
		       count(*) FILTER (WHERE status = 'SUCCEEDED'),
		       count(*) FILTER (WHERE status = 'FAILED'),
		       COALESCE(EXTRACT(EPOCH FROM now() - min(run_at) FILTER (WHERE status = 'PENDING' AND run_at < now()))::bigint, 0)
		  FROM jobs GROUP BY job_type ORDER BY job_type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []controlplane.JobSummaryRow{}
	for rows.Next() {
		var s controlplane.JobSummaryRow
		if err := rows.Scan(&s.JobType, &s.Pending, &s.Running, &s.Succeeded, &s.Failed, &s.OverdueSeconds); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// CountRequests counts buyer requests (optionally of one status), for the
// paginated request list's total.
func (r *ControlPlaneRepository) CountRequests(ctx context.Context, status string) (int, error) {
	var n int
	if status != "" {
		err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM buyer_requests WHERE status = $1`, status).Scan(&n)
		return n, err
	}
	err := r.db.QueryRowContext(ctx, `SELECT count(*) FROM buyer_requests`).Scan(&n)
	return n, err
}
