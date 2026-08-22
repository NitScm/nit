package queue

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/NitScm/nit/internal/store"
	"github.com/NitScm/nit/pkg/protocol"
)

// Handler executes a claimed task and returns its marshalled result.
//
// The context passed to a handler is the handle's: it is cancelled if the lease
// is lost, and every long operation must honour it. A handler that ignores it
// can keep cloning and pushing after another worker has taken over the same
// task.
type Handler func(ctx context.Context, task *store.Task) ([]byte, error)

// Runner polls a queue and executes tasks with a handler.
type Runner struct {
	queue   *Queue
	holder  string
	kinds   []protocol.TaskKind
	handler Handler
	log     *slog.Logger
}

// NewRunner returns a runner. holder identifies this worker in leases and logs;
// kinds restricts what it will take, which is how a deployment dedicates
// machines to one queue.
func NewRunner(q *Queue, holder string, handler Handler, log *slog.Logger, kinds ...protocol.TaskKind) *Runner {
	if log == nil {
		log = slog.Default()
	}

	return &Runner{
		queue:   q,
		holder:  holder,
		kinds:   kinds,
		handler: handler,
		log:     log,
	}
}

// Run polls until the context is cancelled.
//
// One task is executed at a time. Concurrency comes from running several
// runners, not from fanning out inside one: a worker that runs many tasks at
// once has to share its clone cache and its disk budget between them, and the
// failure modes that creates are worse than the throughput it buys.
func (r *Runner) Run(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}

		handle, err := r.queue.Claim(ctx, r.holder, r.kinds...)

		switch {
		case errors.Is(err, store.ErrNoTask):
			if !sleep(ctx, r.queue.opts.PollEvery) {
				return nil
			}
			continue

		case err != nil:
			r.log.ErrorContext(ctx, "claim failed", "error", err)

			if !sleep(ctx, r.queue.opts.PollEvery) {
				return nil
			}
			continue
		}

		r.execute(ctx, handle)
	}
}

func (r *Runner) execute(ctx context.Context, h *Handle) {
	log := r.log.With(
		"task", h.Task.ID,
		"kind", h.Task.Kind,
		"repository", h.Task.RepositoryID,
		"branch", h.Task.Branch,
		"attempt", h.Task.Attempts,
	)

	log.InfoContext(ctx, "task started")

	result, err := r.handler(h.Context(), h.Task)

	// A shutdown must not be recorded as a task failure: the task never ran to
	// completion and deserves another worker, not a strike against its attempt
	// budget.
	if err != nil && ctx.Err() != nil && errors.Is(err, context.Canceled) {
		if releaseErr := h.Release(context.WithoutCancel(ctx)); releaseErr != nil {
			log.ErrorContext(ctx, "release failed", "error", releaseErr)
		}
		log.InfoContext(ctx, "task released on shutdown")
		return
	}

	if err != nil {
		requeued, failErr := h.Fail(context.WithoutCancel(ctx), asProtocolError(err))
		if failErr != nil {
			log.ErrorContext(ctx, "fail failed", "error", failErr)
			return
		}

		log.WarnContext(ctx, "task failed", "error", err, "requeued", requeued)
		return
	}

	if err := h.Complete(context.WithoutCancel(ctx), result); err != nil {
		log.ErrorContext(ctx, "complete failed", "error", err)
		return
	}

	log.InfoContext(ctx, "task succeeded")
}

// asProtocolError maps a handler error onto the wire representation. A handler
// that already produced a protocol.Error keeps its code, so the client sees the
// specific reason rather than a generic failure.
func asProtocolError(err error) *protocol.Error {
	var perr *protocol.Error
	if errors.As(err, &perr) {
		return perr
	}

	return &protocol.Error{
		Code:    "internal",
		Message: err.Error(),
	}
}

// sleep waits for d or until ctx is done. It reports false when the context
// ended, so callers can return without a second check.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// Reaper periodically returns tasks abandoned by dead workers to the queue.
type Reaper struct {
	queue    *Queue
	interval time.Duration
	log      *slog.Logger
}

// NewReaper returns a reaper running every interval.
func NewReaper(q *Queue, interval time.Duration, log *slog.Logger) *Reaper {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	if log == nil {
		log = slog.Default()
	}

	return &Reaper{queue: q, interval: interval, log: log}
}

// Run reaps until the context is cancelled.
func (r *Reaper) Run(ctx context.Context) error {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil

		case <-ticker.C:
			released, err := r.queue.ReapExpired(ctx)
			if err != nil {
				r.log.ErrorContext(ctx, "reap failed", "error", err)
				continue
			}
			if released > 0 {
				r.log.WarnContext(ctx, "released stranded tasks", "count", released)
			}
		}
	}
}

// Permanent wraps an error so the queue will not retry it.
func Permanent(code, message string) error {
	return &protocol.Error{Code: code, Message: message}
}
