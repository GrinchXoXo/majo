package jobs

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/middleman/backend/internal/event"
)

// memStore is an in-memory jobs.Store for worker unit tests. The Postgres
// implementation (row locks, leases, SKIP LOCKED) is covered by the
// integration tests in infrastructure/postgres.
type memStore struct {
	mu   sync.Mutex
	jobs []*Job
	now  time.Time
	// failEvents are the events stored together with a FAILED transition.
	failEvents []*event.Event
	// rejectEvents makes Fail refuse any call that carries an event.
	rejectEvents bool
}

func (m *memStore) Enqueue(ctx context.Context, j *Job) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.jobs {
		if x.Type == j.Type && x.IdempotencyKey == j.IdempotencyKey {
			return false, nil
		}
	}
	c := *j
	c.ID, c.Status, c.RunAt = uuid.New(), StatusPending, m.now
	m.jobs = append(m.jobs, &c)
	return true, nil
}

func (m *memStore) Claim(ctx context.Context, types []string, workerID string, lease time.Duration) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, j := range m.jobs {
		if j.Status == StatusPending && !j.RunAt.After(m.now) {
			j.Status, j.Attempts, j.LockedBy = StatusRunning, j.Attempts+1, workerID
			c := *j
			return &c, nil
		}
	}
	return nil, nil
}

func (m *memStore) find(id uuid.UUID, worker string) (*Job, error) {
	for _, j := range m.jobs {
		if j.ID == id {
			if j.Status != StatusRunning || j.LockedBy != worker {
				return nil, ErrLeaseLost
			}
			return j, nil
		}
	}
	return nil, errors.New("not found")
}

func (m *memStore) Complete(ctx context.Context, id uuid.UUID, w string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.find(id, w)
	if err == nil {
		j.Status, j.LockedBy = StatusSucceeded, ""
	}
	return err
}

func (m *memStore) Retry(ctx context.Context, id uuid.UUID, w string, runAt time.Time, e string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	j, err := m.find(id, w)
	if err == nil {
		j.Status, j.RunAt, j.LastError, j.LockedBy = StatusPending, runAt, e, ""
	}
	return err
}

func (m *memStore) Fail(ctx context.Context, id uuid.UUID, w string, e string, ev *event.Event) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ev != nil && m.rejectEvents {
		return errors.New("event write failed")
	}
	j, err := m.find(id, w)
	if err == nil {
		j.Status, j.LastError, j.LockedBy = StatusFailed, e, ""
		if ev != nil {
			m.failEvents = append(m.failEvents, ev)
		}
	}
	return err
}

func newTestWorker(store *memStore, h Handler, onFail FailureListener) *Worker {
	w := NewWorker(store, WorkerConfig{ID: "t", Retry: RetryPolicy{BaseDelay: time.Second, MaxDelay: time.Minute}},
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	w.now = func() time.Time { return store.now }
	w.Register("T", h, onFail)
	return w
}

func TestWorker_SuccessCompletesJob(t *testing.T) {
	s := &memStore{now: time.Now()}
	_, _ = s.Enqueue(context.Background(), &Job{Type: "T", IdempotencyKey: "k", MaxAttempts: 3})
	w := newTestWorker(s, func(ctx context.Context, j *Job) error { return nil }, nil)
	ran, err := w.RunOnce(context.Background(), "t/0")
	if !ran || err != nil || s.jobs[0].Status != StatusSucceeded || s.jobs[0].Attempts != 1 {
		t.Fatalf("ran=%v err=%v job=%+v", ran, err, s.jobs[0])
	}
}

func TestWorker_EnqueueIsIdempotentPerKey(t *testing.T) {
	s := &memStore{now: time.Now()}
	a, _ := s.Enqueue(context.Background(), &Job{Type: "T", IdempotencyKey: "k", MaxAttempts: 3})
	b, _ := s.Enqueue(context.Background(), &Job{Type: "T", IdempotencyKey: "k", MaxAttempts: 3})
	if !a || b || len(s.jobs) != 1 {
		t.Fatalf("expected one job, created=%v,%v n=%d", a, b, len(s.jobs))
	}
}

func TestWorker_TransientErrorsRetryWithBackoffThenSucceed(t *testing.T) {
	s := &memStore{now: time.Now()}
	_, _ = s.Enqueue(context.Background(), &Job{Type: "T", IdempotencyKey: "k", MaxAttempts: 5})
	calls := 0
	w := newTestWorker(s, func(ctx context.Context, j *Job) error {
		calls++
		if calls <= 2 {
			return errors.New("503 from provider")
		}
		return nil
	}, nil)

	for i := 0; i < 3; i++ {
		ran, _ := w.RunOnce(context.Background(), "t/0")
		if !ran {
			t.Fatalf("iteration %d: expected a due job", i)
		}
		j := s.jobs[0]
		if i < 2 {
			if j.Status != StatusPending || j.LastError != "503 from provider" || !j.RunAt.After(s.now) {
				t.Fatalf("expected retry scheduled in the future, got %+v", j)
			}
			// Not due yet: nothing to claim until time advances.
			if ran, _ := w.RunOnce(context.Background(), "t/0"); ran {
				t.Fatal("job ran before its backoff elapsed")
			}
			s.now = j.RunAt
		}
	}
	if j := s.jobs[0]; j.Status != StatusSucceeded || j.Attempts != 3 {
		t.Fatalf("expected success on 3rd attempt, got %+v", j)
	}
}

func TestWorker_PermanentErrorFailsImmediatelyAndNotifies(t *testing.T) {
	s := &memStore{now: time.Now()}
	_, _ = s.Enqueue(context.Background(), &Job{Type: "T", IdempotencyKey: "k", MaxAttempts: 5})
	var notified *Job
	w := newTestWorker(s, func(ctx context.Context, j *Job) error {
		return Permanent(errors.New("404 media not found"))
	}, func(ctx context.Context, j *Job, err error) { notified = j })
	_, _ = w.RunOnce(context.Background(), "t/0")
	j := s.jobs[0]
	if j.Status != StatusFailed || j.Attempts != 1 || j.LastError != "404 media not found" {
		t.Fatalf("expected terminal failure after one attempt, got %+v", j)
	}
	if notified == nil || notified.Status != StatusFailed {
		t.Fatal("expected failure listener to be called")
	}
}

func TestWorker_RetriesAreBoundedByMaxAttempts(t *testing.T) {
	s := &memStore{now: time.Now()}
	_, _ = s.Enqueue(context.Background(), &Job{Type: "T", IdempotencyKey: "k", MaxAttempts: 3})
	w := newTestWorker(s, func(ctx context.Context, j *Job) error { return errors.New("timeout") }, nil)
	for i := 0; i < 10; i++ {
		if ran, _ := w.RunOnce(context.Background(), "t/0"); !ran {
			break
		}
		s.now = s.jobs[0].RunAt
	}
	j := s.jobs[0]
	if j.Status != StatusFailed || j.Attempts != 3 {
		t.Fatalf("expected FAILED after exactly 3 attempts, got %+v", j)
	}
	if want := "attempts exhausted (3/3): timeout"; j.LastError != want {
		t.Fatalf("last_error = %q, want %q", j.LastError, want)
	}
}

func TestWorker_PanicIsRetriedNotCrash(t *testing.T) {
	s := &memStore{now: time.Now()}
	_, _ = s.Enqueue(context.Background(), &Job{Type: "T", IdempotencyKey: "k", MaxAttempts: 2})
	w := newTestWorker(s, func(ctx context.Context, j *Job) error { panic("boom") }, nil)
	_, _ = w.RunOnce(context.Background(), "t/0")
	if j := s.jobs[0]; j.Status != StatusPending || j.LastError == "" {
		t.Fatalf("expected retry after panic, got %+v", j)
	}
}

func TestWorker_ReclaimedPastFinalAttemptFailsWithoutRunning(t *testing.T) {
	s := &memStore{now: time.Now()}
	_, _ = s.Enqueue(context.Background(), &Job{Type: "T", IdempotencyKey: "k", MaxAttempts: 2})
	s.jobs[0].Attempts = 2 // crashed during the final attempt; lease expired
	ran := false
	w := newTestWorker(s, func(ctx context.Context, j *Job) error { ran = true; return nil }, nil)
	_, _ = w.RunOnce(context.Background(), "t/0")
	if ran || s.jobs[0].Status != StatusFailed {
		t.Fatalf("expected terminal failure without running, ran=%v job=%+v", ran, s.jobs[0])
	}
}

func TestRetryPolicy_ExponentialCappedWithJitter(t *testing.T) {
	p := RetryPolicy{BaseDelay: 10 * time.Second, MaxDelay: 60 * time.Second}
	rnd := rand.New(rand.NewSource(1))
	for attempt, want := range map[int]time.Duration{1: 10 * time.Second, 2: 20 * time.Second, 3: 40 * time.Second, 4: 60 * time.Second, 9: 60 * time.Second} {
		for i := 0; i < 50; i++ {
			d := p.Delay(attempt, rnd)
			if d < want/2 || d > want {
				t.Fatalf("attempt %d: delay %v outside [%v, %v]", attempt, d, want/2, want)
			}
		}
	}
}

func TestIsPermanent(t *testing.T) {
	if IsPermanent(errors.New("x")) || !IsPermanent(Permanent(errors.New("x"))) {
		t.Fatal("Permanent wrapper not detected")
	}
	wrapped := errors.Join(errors.New("ctx"), Permanent(errors.New("x")))
	if !IsPermanent(wrapped) {
		t.Fatal("wrapped permanent error not detected")
	}
}

func TestWorker_GracefulShutdownFinishesInFlightJob(t *testing.T) {
	s := &memStore{now: time.Now()}
	_, _ = s.Enqueue(context.Background(), &Job{Type: "T", IdempotencyKey: "k", MaxAttempts: 3})
	started, release := make(chan struct{}), make(chan struct{})
	w := newTestWorker(s, func(ctx context.Context, j *Job) error {
		close(started)
		<-release // still working when shutdown is requested
		return nil
	}, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("Run returned before the in-flight job finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after shutdown")
	}
	if s.jobs[0].Status != StatusSucceeded {
		t.Fatalf("in-flight job should be completed despite shutdown, got %s", s.jobs[0].Status)
	}
	// No new job is claimed after shutdown.
	_, _ = s.Enqueue(context.Background(), &Job{Type: "T", IdempotencyKey: "k2", MaxAttempts: 3})
	if ran, _ := w.RunOnce(ctx, "t/0"); ran {
		t.Fatal("a stopped worker must not claim new jobs")
	}
}

func TestWorker_FailureEventIsStoredWithTheFailedTransition(t *testing.T) {
	s := &memStore{now: time.Now()}
	_, _ = s.Enqueue(context.Background(), &Job{Type: "T", IdempotencyKey: "k", MaxAttempts: 2})
	w := newTestWorker(s, func(ctx context.Context, j *Job) error { return errors.New("timeout") }, nil)
	var seen *Job
	w.RegisterFailureEvent("T", func(j *Job, err error) *event.Event {
		seen = j
		return &event.Event{EventType: "X_FAILED", Payload: map[string]any{"error": j.LastError}}
	})
	_, _ = w.RunOnce(context.Background(), "t/0") // attempt 1 -> retry: no event
	if len(s.failEvents) != 0 {
		t.Fatal("a retry must not produce the failure event")
	}
	s.now = s.jobs[0].RunAt
	_, _ = w.RunOnce(context.Background(), "t/0") // attempt 2 -> terminal
	if s.jobs[0].Status != StatusFailed || len(s.failEvents) != 1 {
		t.Fatalf("want FAILED + exactly one event passed to Store.Fail, got %s / %d", s.jobs[0].Status, len(s.failEvents))
	}
	if seen.Status != StatusFailed || s.failEvents[0].Payload["error"] != "attempts exhausted (2/2): timeout" {
		t.Fatalf("the event sees the terminal state: %+v", s.failEvents[0].Payload)
	}
}

func TestWorker_UnstorableFailureEventStillFailsTheJob(t *testing.T) {
	s := &memStore{now: time.Now(), rejectEvents: true}
	_, _ = s.Enqueue(context.Background(), &Job{Type: "T", IdempotencyKey: "k", MaxAttempts: 1})
	w := newTestWorker(s, func(ctx context.Context, j *Job) error { return Permanent(errors.New("404")) }, nil)
	w.RegisterFailureEvent("T", func(j *Job, err error) *event.Event { return &event.Event{EventType: "X_FAILED"} })
	_, _ = w.RunOnce(context.Background(), "t/0")
	if s.jobs[0].Status != StatusFailed || len(s.failEvents) != 0 {
		t.Fatalf("the job must still reach FAILED (without the event), got %s", s.jobs[0].Status)
	}
}
