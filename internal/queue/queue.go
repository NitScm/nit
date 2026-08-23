// Package queue turns the task store into a work queue with leases.
//
// The design decision it implements is documented in docs/DECISIONS.md D11: nit
// serializes a branch with a queue rather than with a lock held across the whole
// clone-apply-push cycle. A lock held that long blocks a branch for minutes and
// strands it forever if the holder crashes; a queue serializes just as well,
// never refuses a developer, and recovers from worker death by itself.
//
// Three properties matter and are tested:
//
//   - At most one task per partition runs at a time, so two pushes to the same
//     branch cannot be applied concurrently.
//   - A lease expires. A worker that dies mid-task releases its branch without
//     anyone intervening.
//   - A lease is fenced. A worker whose lease lapsed cannot complete a task
//     another worker has since picked up, and it learns about it: the handle's
//     context is cancelled, so a long clone aborts instead of pushing work
//     nobody owns any more.
package queue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// Options configures a queue.
type Options struct {
	// LeaseFor is how long a claim is valid without a heartbeat.
	LeaseFor time.Duration

	// HeartbeatEvery is the interval between automatic lease renewals. It must
	// be comfortably shorter than LeaseFor: a renewal that only just beats the
	// deadline will lose the race on a loaded machine.
	HeartbeatEvery time.Duration

	// MaxAttempts caps retries of a failing task. Beyond it the task is failed
	// terminally rather than cycling through the queue forever.
	MaxAttempts int

	// PollEvery is how often an idle worker asks for work.
	//
	// A production deployment should back this with LISTEN/NOTIFY so a queued
	// task is picked up immediately; polling is the floor, not the mechanism.
	PollEvery time.Duration

	// Clock is the time source, injectable so lease behaviour can be tested
	// without sleeping.
	Clock func() time.Time
}

func (o *Options) applyDefaults() {
	if o.LeaseFor <= 0 {
		o.LeaseFor = 60 * time.Second
	}
	if o.HeartbeatEvery <= 0 {
		o.HeartbeatEvery = o.LeaseFor / 3
	}
	if o.MaxAttempts <= 0 {
		o.MaxAttempts = 3
	}
	if o.PollEvery <= 0 {
		o.PollEvery = time.Second
	}
	if o.Clock == nil {
		o.Clock = func() time.Time { return time.Now().UTC() }
	}
}

// Queue dispatches tasks to workers.
type Queue struct {
	tasks store.TaskStore
	opts  Options
}

// New returns a queue over a task store.
func New(tasks store.TaskStore, opts Options) *Queue {
	opts.applyDefaults()
	return &Queue{tasks: tasks, opts: opts}
}

// PartitionKey returns the serialization key for a task.
//
// Pushes serialize per repository and branch. Pulls are read-only and get no
// key at all, so they run fully in parallel — which is the whole reason the key
// is computed here rather than assumed by the store.
func PartitionKey(kind protocol.TaskKind, repository, branch string) string {
	if kind != protocol.TaskPush {
		return ""
	}
	return repository + ":" + branch
}

// Submit enqueues a task, idempotently.
//
// A repeated request id returns the existing task and reports existing=true,
// rather than failing: a client retrying after a network error wants its
// original task back, not an error, and certainly not a second upstream commit.
func (q *Queue) Submit(ctx context.Context, t *store.Task) (task *store.Task, existing bool, err error) {
	if t.CreatedAt.IsZero() {
		t.CreatedAt = q.opts.Clock()
	}
	if t.State == "" {
		t.State = protocol.TaskQueued
	}

	created, err := q.tasks.Create(ctx, t)

	switch {
	case errors.Is(err, store.ErrDuplicateRequest):
		if created != nil {
			return created, true, nil
		}

		found, lookupErr := q.tasks.ByRequestID(ctx, t.TenantID, t.RequestID)
		if lookupErr != nil {
			return nil, false, lookupErr
		}
		return found, true, nil

	case err != nil:
		return nil, false, err
	}

	return created, false, nil
}

// Handle is a claimed task, held by one worker.
//
// While it is open, the queue renews the lease in the background. If renewal
// fails — the lease lapsed and another worker took over — the handle's context
// is cancelled so the worker stops immediately.
type Handle struct {
	// Task is the claimed task as it stood at claim time.
	Task *store.Task

	queue *Queue
	token string

	ctx    context.Context
	cancel context.CancelCauseFunc

	stopOnce sync.Once
	stopped  chan struct{}
	done     chan struct{}
}

// ErrLeaseLost is the cause set on a handle's context when the lease is lost.
var ErrLeaseLost = errors.New("queue: lease lost")

// Context returns a context cancelled when the lease is lost or the handle is
// finished. Workers must pass it to every long operation they perform: a clone
// that keeps running after its lease expired is doing work another worker is
// already redoing.
func (h *Handle) Context() context.Context { return h.ctx }

// Claim takes the next dispatchable task and starts renewing its lease.
//
// Returns store.ErrNoTask when nothing is available; that is the normal state
// of an idle worker, not an error condition.
func (q *Queue) Claim(ctx context.Context, holder string, kinds ...protocol.TaskKind) (*Handle, error) {
	now := q.opts.Clock()

	task, err := q.tasks.Claim(ctx, store.ClaimOptions{
		Holder:   holder,
		Kinds:    kinds,
		LeaseFor: q.opts.LeaseFor,
		Now:      now,
	})
	if err != nil {
		return nil, err
	}

	// The parent context is deliberately not the request context: a handle
	// outlives the call that created it, and must only be cancelled by lease
	// loss or by the worker finishing.
	handleCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))

	h := &Handle{
		Task:    task,
		queue:   q,
		token:   task.Lease.Token,
		ctx:     handleCtx,
		cancel:  cancel,
		stopped: make(chan struct{}),
		done:    make(chan struct{}),
	}

	go h.keepAlive()

	return h, nil
}

// keepAlive renews the lease until the handle is finished or the lease is lost.
func (h *Handle) keepAlive() {
	defer close(h.done)

	ticker := time.NewTicker(h.queue.opts.HeartbeatEvery)
	defer ticker.Stop()

	for {
		select {
		case <-h.stopped:
			return

		case <-ticker.C:
			until := h.queue.opts.Clock().Add(h.queue.opts.LeaseFor)

			err := h.queue.tasks.Heartbeat(h.ctx, h.Task.ID, h.token, until)
			if err == nil {
				continue
			}

			// Any heartbeat failure is treated as lease loss. Continuing to
			// work on a task whose ownership is in doubt risks two workers
			// pushing the same change.
			h.cancel(fmt.Errorf("%w: %v", ErrLeaseLost, err))
			return
		}
	}
}

// stop ends lease renewal and waits for the goroutine to exit.
func (h *Handle) stop(cause error) {
	h.stopOnce.Do(func() {
		close(h.stopped)
		<-h.done
		h.cancel(cause)
	})
}

// ErrFinished is the cause set on a handle's context once the task is done.
var ErrFinished = errors.New("queue: task finished")

// Complete marks the task succeeded.
func (h *Handle) Complete(ctx context.Context, result []byte) error {
	h.stop(ErrFinished)
	return h.queue.tasks.Complete(ctx, h.Task.ID, h.token, result, h.queue.opts.Clock())
}

// Fail marks the task failed, requeueing it if attempts remain.
//
// Returns whether the task was requeued, so a worker can log the difference
// between "will be retried" and "gave up".
func (h *Handle) Fail(ctx context.Context, cause *protocol.Error) (requeued bool, err error) {
	h.stop(ErrFinished)

	requeue := h.Task.Attempts < h.queue.opts.MaxAttempts

	// A permanent failure is never worth retrying: the same patch will be
	// refused by the same rules, and the same conflict will still conflict.
	if cause != nil && isPermanent(cause.Code) {
		requeue = false
	}

	if err := h.queue.tasks.Fail(ctx, h.Task.ID, h.token, cause, requeue, h.queue.opts.Clock()); err != nil {
		return false, err
	}

	return requeue, nil
}

// Release puts the task back on the queue without recording a failure. It is
// what a worker calls when it is shutting down cleanly.
func (h *Handle) Release(ctx context.Context) error {
	h.stop(ErrFinished)
	return h.queue.tasks.Fail(ctx, h.Task.ID, h.token, nil, true, h.queue.opts.Clock())
}

// isPermanent reports whether an error code describes a condition a retry
// cannot fix.
func isPermanent(code string) bool {
	switch code {
	case protocol.CodeUnauthorizedPaths,
		protocol.CodeUnknownRepository,
		protocol.CodeStaleSyncPoint,
		protocol.CodeUnknownSyncPoint,
		protocol.CodePatchTooLarge,
		protocol.CodeUnsupportedVersion:
		return true
	default:
		return false
	}
}

// ReapExpired returns tasks abandoned by dead workers to the queue.
//
// Claim already reclaims expired leases opportunistically, so this exists for
// the case where nothing is claiming: a queue that has gone quiet still needs
// its stranded tasks released for the metrics and the operator view to be
// truthful.
func (q *Queue) ReapExpired(ctx context.Context) (int, error) {
	return q.tasks.ReleaseExpired(ctx, q.opts.Clock())
}

// Position reports how many tasks sit ahead of this one in its partition.
func (q *Queue) Position(ctx context.Context, id store.ID) (int, error) {
	return q.tasks.QueuePosition(ctx, id)
}
