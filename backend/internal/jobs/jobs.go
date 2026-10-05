// Package jobs provides a lightweight in-process background job runner.
// Per PRD 1 §12 and PRD 0.2 §19: a simple goroutine-based mechanism is
// sufficient for Phase 1's background extraction work. Redis/queue
// infrastructure is deferred until there's measurable evidence of need.
package jobs

import (
	"context"
	"log/slog"
)

// Runner executes jobs on a bounded worker pool so a burst of inbound
// WhatsApp messages can't spawn unbounded goroutines.
type Runner struct {
	queue  chan func(context.Context)
	logger *slog.Logger
}

func NewRunner(logger *slog.Logger, workers, bufferSize int) *Runner {
	r := &Runner{
		queue:  make(chan func(context.Context), bufferSize),
		logger: logger,
	}
	for i := 0; i < workers; i++ {
		go r.worker()
	}
	return r
}

func (r *Runner) worker() {
	for job := range r.queue {
		func() {
			defer func() {
				if rec := recover(); rec != nil {
					r.logger.Error("background job panicked", "recover", rec)
				}
			}()
			job(context.Background())
		}()
	}
}

// Enqueue schedules a job for background execution. If the queue is full,
// the job runs synchronously as a safety fallback rather than being
// dropped — losing a buyer's request is never acceptable (PRD 1 §20).
func (r *Runner) Enqueue(job func(context.Context)) {
	select {
	case r.queue <- job:
	default:
		r.logger.Warn("job queue full, running synchronously")
		job(context.Background())
	}
}
