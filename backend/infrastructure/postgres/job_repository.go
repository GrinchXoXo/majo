package postgres

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"

	"github.com/middleman/backend/internal/jobs"
)

// JobRepository is the PostgreSQL implementation of jobs.Store (Phase 4A.1).
// Ownership of a job is decided by the database: Claim takes a row lock
// with FOR UPDATE SKIP LOCKED and stamps locked_by/locked_at, and every
// later transition is fenced on locked_by so a worker whose lease expired
// cannot overwrite the state of the worker that reclaimed the job.
type JobRepository struct {
	db *sql.DB
}

func NewJobRepository(db *sql.DB) *JobRepository { return &JobRepository{db: db} }

const jobColumns = `id, job_type, idempotency_key, payload, status, attempts, max_attempts, run_at,
	COALESCE(locked_by, ''), COALESCE(last_error, ''), created_at, updated_at, completed_at`

func scanJob(row interface{ Scan(...any) error }) (*jobs.Job, error) {
	var (
		j       jobs.Job
		idStr   string
		payload []byte
		done    sql.NullTime
	)
	if err := row.Scan(&idStr, &j.Type, &j.IdempotencyKey, &payload, &j.Status, &j.Attempts, &j.MaxAttempts,
		&j.RunAt, &j.LockedBy, &j.LastError, &j.CreatedAt, &j.UpdatedAt, &done); err != nil {
		return nil, err
	}
	j.ID, _ = uuid.Parse(idStr)
	if err := json.Unmarshal(payload, &j.Payload); err != nil || j.Payload == nil {
		j.Payload = map[string]any{}
	}
	if done.Valid {
		t := done.Time
		j.CompletedAt = &t
	}
	return &j, nil
}

func (r *JobRepository) Enqueue(ctx context.Context, j *jobs.Job) (bool, error) {
	if j.ID == uuid.Nil {
		j.ID = uuid.New()
	}
	if j.Payload == nil {
		j.Payload = map[string]any{}
	}
	payload, err := json.Marshal(j.Payload)
	if err != nil {
		return false, err
	}
	var runAt any
	if !j.RunAt.IsZero() {
		runAt = j.RunAt
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO jobs (id, job_type, idempotency_key, payload, max_attempts, run_at)
		VALUES ($1, $2, $3, $4, $5, COALESCE($6, now()))
		ON CONFLICT (job_type, idempotency_key) DO NOTHING
	`, j.ID.String(), j.Type, j.IdempotencyKey, payload, j.MaxAttempts, runAt)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func (r *JobRepository) Claim(ctx context.Context, types []string, workerID string, lease time.Duration) (*jobs.Job, error) {
	if len(types) == 0 {
		return nil, nil
	}
	row := r.db.QueryRowContext(ctx, `
		UPDATE jobs
		SET status = 'RUNNING', attempts = attempts + 1, locked_at = now(), locked_by = $1, updated_at = now()
		WHERE id = (
			SELECT id FROM jobs
			WHERE job_type = ANY($2)
			  AND (   (status = 'PENDING' AND run_at <= now())
			       OR (status = 'RUNNING' AND locked_at < now() - make_interval(secs => $3)))
			ORDER BY run_at
			FOR UPDATE SKIP LOCKED
			LIMIT 1
		)
		RETURNING `+jobColumns,
		workerID, pq.Array(types), lease.Seconds())
	j, err := scanJob(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

func (r *JobRepository) transition(ctx context.Context, q string, args ...any) error {
	res, err := r.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return jobs.ErrLeaseLost
	}
	return err
}

func (r *JobRepository) Complete(ctx context.Context, id uuid.UUID, workerID string) error {
	return r.transition(ctx, `
		UPDATE jobs SET status = 'SUCCEEDED', completed_at = now(), updated_at = now(),
		       locked_at = NULL, locked_by = NULL, last_error = NULL
		WHERE id = $1 AND status = 'RUNNING' AND locked_by = $2`, id.String(), workerID)
}

func (r *JobRepository) Retry(ctx context.Context, id uuid.UUID, workerID string, runAt time.Time, lastErr string) error {
	return r.transition(ctx, `
		UPDATE jobs SET status = 'PENDING', run_at = $3, last_error = $4, updated_at = now(),
		       locked_at = NULL, locked_by = NULL
		WHERE id = $1 AND status = 'RUNNING' AND locked_by = $2`, id.String(), workerID, runAt, truncate(lastErr))
}

func (r *JobRepository) Fail(ctx context.Context, id uuid.UUID, workerID string, lastErr string) error {
	return r.transition(ctx, `
		UPDATE jobs SET status = 'FAILED', last_error = $3, completed_at = now(), updated_at = now(),
		       locked_at = NULL, locked_by = NULL
		WHERE id = $1 AND status = 'RUNNING' AND locked_by = $2`, id.String(), workerID, truncate(lastErr))
}

// FindByKey returns the job for (type, idempotency key), or nil. Used by
// tests and operational inspection.
func (r *JobRepository) FindByKey(ctx context.Context, jobType, key string) (*jobs.Job, error) {
	j, err := scanJob(r.db.QueryRowContext(ctx,
		`SELECT `+jobColumns+` FROM jobs WHERE job_type = $1 AND idempotency_key = $2`, jobType, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// truncate bounds stored error text (provider error bodies can be long).
func truncate(s string) string {
	const max = 2000
	if len(s) > max {
		return s[:max]
	}
	return s
}
