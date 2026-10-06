package jobs

// Durable jobs (Phase 4A.1). The in-memory Runner above stays as it is for
// the existing extraction work; durable jobs are for work that must survive
// a process restart (first user: inbound media download).
//
// Semantics: at-least-once execution with idempotent effects. PostgreSQL is
// the authority for who owns a job (row lock + lease), never an in-memory
// mutex. There is no exactly-once guarantee and handlers must not assume one.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/event"
)

// Status of a durable job.
type Status string

const (
	StatusPending   Status = "PENDING"
	StatusRunning   Status = "RUNNING"
	StatusSucceeded Status = "SUCCEEDED"
	StatusFailed    Status = "FAILED"
)

// Job is one durable unit of work.
type Job struct {
	ID             uuid.UUID
	Type           string
	IdempotencyKey string
	Payload        map[string]any
	Status         Status
	Attempts       int // incremented on every claim, so it includes the current run
	MaxAttempts    int
	RunAt          time.Time
	LockedBy       string
	LastError      string
	CreatedAt      time.Time
	UpdatedAt      time.Time
	CompletedAt    *time.Time
}

// ErrLeaseLost is returned by Complete/Retry/Fail when the job is no longer
// held by this worker (its lease expired and another worker reclaimed it).
// The effects are idempotent, so the other worker's run is safe.
var ErrLeaseLost = errors.New("jobs: lease lost")

// Store is the persistence boundary for durable jobs.
type Store interface {
	// Enqueue inserts a job unless one with the same (type, idempotency key)
	// already exists. created=false means it already existed (no-op).
	Enqueue(ctx context.Context, j *Job) (created bool, err error)
	// Claim atomically takes one due job of the given types: a PENDING job
	// whose run_at has passed, or a RUNNING job whose lease is older than
	// lease (its worker died). Returns nil, nil when nothing is due.
	Claim(ctx context.Context, types []string, workerID string, lease time.Duration) (*Job, error)
	// Complete marks the job SUCCEEDED.
	Complete(ctx context.Context, id uuid.UUID, workerID string) error
	// Retry returns the job to PENDING with a new run_at and the error.
	Retry(ctx context.Context, id uuid.UUID, workerID string, runAt time.Time, lastErr string) error
	// Fail marks the job FAILED (terminal) with the error. The row is kept.
	// If ev is non-nil it is written in the SAME transaction, so a crash can
	// never leave a FAILED job without its failure event (or the reverse).
	Fail(ctx context.Context, id uuid.UUID, workerID string, lastErr string, ev *event.Event) error
}

// Handler executes one job. Returning nil completes the job. Returning an
// error wrapped with Permanent (or any error with a Permanent() bool method
// that reports true) fails it terminally; any other error is retried with
// backoff until MaxAttempts is reached.
type Handler func(ctx context.Context, j *Job) error

// FailureListener is told when a job fails terminally, after the failure is
// persisted, for follow-up work that may be lost on a crash. Optional.
type FailureListener func(ctx context.Context, j *Job, err error)

// FailureEvent builds the domain event recording a terminal failure. It is
// persisted atomically with the FAILED transition. j.LastError and
// j.Status are already set to their terminal values. Optional; nil = none.
type FailureEvent func(j *Job, err error) *event.Event

type permanentError struct{ err error }

func (p permanentError) Error() string   { return p.err.Error() }
func (p permanentError) Unwrap() error   { return p.err }
func (p permanentError) Permanent() bool { return true }

// Permanent marks err as non-retryable.
func Permanent(err error) error {
	if err == nil {
		return nil
	}
	return permanentError{err}
}

// IsPermanent reports whether err (or anything it wraps) is non-retryable.
func IsPermanent(err error) bool {
	var p interface{ Permanent() bool }
	return errors.As(err, &p) && p.Permanent()
}

// RetryPolicy is bounded exponential backoff with jitter.
type RetryPolicy struct {
	BaseDelay time.Duration // delay after the first failed attempt
	MaxDelay  time.Duration // cap
}

// Delay returns the wait before the next attempt, given how many attempts
// have run. "Equal jitter": half fixed, half random, so retries from a
// burst don't all land at once but never shrink to ~0.
func (p RetryPolicy) Delay(attempts int, rnd *rand.Rand) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := p.BaseDelay
	for i := 1; i < attempts && d < p.MaxDelay; i++ {
		d *= 2
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	if d <= 1 {
		return d
	}
	half := d / 2
	return half + time.Duration(rnd.Int63n(int64(half)+1))
}

// WorkerConfig holds the operational parameters of a Worker.
type WorkerConfig struct {
	ID           string // identifies this worker in locked_by; defaults to a random ID
	PollInterval time.Duration
	Lease        time.Duration // a RUNNING job older than this is reclaimable
	Concurrency  int
	Retry        RetryPolicy
}

// Worker polls the Store, claims due jobs and runs their handlers.
type Worker struct {
	store     Store
	cfg       WorkerConfig
	handlers  map[string]Handler
	onFailure map[string]FailureListener
	failEvent map[string]FailureEvent
	logger    *slog.Logger

	mu  sync.Mutex
	rnd *rand.Rand
	now func() time.Time
}

func NewWorker(store Store, cfg WorkerConfig, logger *slog.Logger) *Worker {
	if cfg.ID == "" {
		cfg.ID = "worker-" + uuid.NewString()[:8]
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 2 * time.Second
	}
	if cfg.Lease <= 0 {
		cfg.Lease = 5 * time.Minute
	}
	return &Worker{
		store: store, cfg: cfg, handlers: map[string]Handler{}, onFailure: map[string]FailureListener{}, failEvent: map[string]FailureEvent{},
		logger: logger, rnd: rand.New(rand.NewSource(time.Now().UnixNano())), now: time.Now,
	}
}

// RegisterFailureEvent sets the event written atomically with a terminal
// failure of jobType (Phase 4A Step 4 follow-up).
func (w *Worker) RegisterFailureEvent(jobType string, fn FailureEvent) {
	w.failEvent[jobType] = fn
}

// Register binds a handler (and optional terminal-failure listener) to a job type.
func (w *Worker) Register(jobType string, h Handler, onFailure FailureListener) {
	w.handlers[jobType] = h
	if onFailure != nil {
		w.onFailure[jobType] = onFailure
	}
}

// Run polls until ctx is cancelled, with cfg.Concurrency loops. It returns
// after every loop has finished its current job.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for i := 0; i < w.cfg.Concurrency; i++ {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			id := fmt.Sprintf("%s/%d", w.cfg.ID, slot)
			for {
				ran, err := w.RunOnce(ctx, id)
				if err != nil && ctx.Err() == nil {
					w.logger.Error("durable job poll failed", "worker", id, "error", err)
				}
				if ran && err == nil {
					continue // drain: there may be more due work
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(w.cfg.PollInterval):
				}
			}
		}(i)
	}
	wg.Wait()
}

// RunOnce claims and processes at most one job. ran reports whether a job
// was claimed. Exposed for tests and deterministic draining.
func (w *Worker) RunOnce(ctx context.Context, workerID string) (ran bool, err error) {
	if ctx.Err() != nil {
		return false, nil
	}
	types := make([]string, 0, len(w.handlers))
	for t := range w.handlers {
		types = append(types, t)
	}
	j, err := w.store.Claim(ctx, types, workerID, w.cfg.Lease)
	if err != nil || j == nil {
		return false, err
	}
	w.process(ctx, j, workerID)
	return true, nil
}

func (w *Worker) process(ctx context.Context, j *Job, workerID string) {
	log := w.logger.With("job_id", j.ID, "job_type", j.Type, "attempt", j.Attempts, "max_attempts", j.MaxAttempts)
	// Bookkeeping must still reach the database if shutdown cancels ctx
	// mid-job; otherwise the job would sit RUNNING until its lease expires.
	bk, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()

	h := w.handlers[j.Type]
	var herr error
	switch {
	case j.Attempts > j.MaxAttempts:
		// Reclaimed after a crash on its final attempt: don't run again.
		herr = Permanent(fmt.Errorf("lease expired on final attempt (%d/%d)", j.Attempts-1, j.MaxAttempts))
	default:
		herr = w.safeRun(ctx, h, j)
	}

	if herr == nil {
		if err := w.store.Complete(bk, j.ID, workerID); err != nil {
			log.Error("completing durable job", "error", err)
			return
		}
		log.Info("durable job succeeded")
		return
	}

	if IsPermanent(herr) || j.Attempts >= j.MaxAttempts {
		reason := herr.Error()
		if !IsPermanent(herr) {
			reason = fmt.Sprintf("attempts exhausted (%d/%d): %s", j.Attempts, j.MaxAttempts, reason)
		}
		j.LastError, j.Status = reason, StatusFailed
		var ev *event.Event
		if fn := w.failEvent[j.Type]; fn != nil {
			ev = fn(j, herr)
		}
		err := w.store.Fail(bk, j.ID, workerID, reason, ev)
		if err != nil && ev != nil && !errors.Is(err, ErrLeaseLost) {
			// The event itself could not be stored (both rolled back). Fail
			// without it rather than leaving the job to be reclaimed forever.
			log.Error("storing failure event; failing job without it", "error", err, "event_type", ev.EventType)
			err = w.store.Fail(bk, j.ID, workerID, reason, nil)
		}
		if err != nil {
			log.Error("failing durable job", "error", err, "job_error", reason)
			return
		}
		log.Error("durable job failed terminally", "error", reason)
		if l := w.onFailure[j.Type]; l != nil {
			l(bk, j, herr)
		}
		return
	}

	w.mu.Lock()
	delay := w.cfg.Retry.Delay(j.Attempts, w.rnd)
	w.mu.Unlock()
	runAt := w.now().Add(delay)
	if err := w.store.Retry(bk, j.ID, workerID, runAt, herr.Error()); err != nil {
		log.Error("scheduling durable job retry", "error", err, "job_error", herr)
		return
	}
	log.Warn("durable job failed, retry scheduled", "error", herr, "retry_in", delay.Round(time.Millisecond))
}

func (w *Worker) safeRun(ctx context.Context, h Handler, j *Job) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("handler panicked: %v", rec)
		}
	}()
	if h == nil {
		return Permanent(fmt.Errorf("no handler registered for job type %q", j.Type))
	}
	return h(ctx, j)
}
