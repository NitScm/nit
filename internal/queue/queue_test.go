package queue_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/queue"
	"github.com/NitScm/nit/internal/store/memory"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// clock is a manually advanced time source, so lease behaviour is tested by
// moving time rather than by sleeping through it.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func newClock() *clock {
	return &clock{now: time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)}
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

type harness struct {
	store *memory.Store
	queue *queue.Queue
	clock *clock
}

func newHarness(t *testing.T, opts queue.Options) *harness {
	t.Helper()

	c := newClock()
	opts.Clock = c.Now

	s := memory.New()

	return &harness{store: s, queue: queue.New(s.Tasks(), opts), clock: c}
}

func (h *harness) task(requestID, branch string, kind protocol.TaskKind) *store.Task {
	return &store.Task{
		TenantID:     policy.DefaultTenant,
		RequestID:    requestID,
		Kind:         kind,
		UserID:       "user-1",
		RepositoryID: "repo-1",
		Branch:       branch,
		PartitionKey: queue.PartitionKey(kind, "repo-1", branch),
		Payload:      []byte(`{}`),
		CreatedAt:    h.clock.Now(),
	}
}

func TestSubmitIsIdempotent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, queue.Options{})

	first, existing, err := h.queue.Submit(ctx, h.task("req-1", "main", protocol.TaskPush))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if existing {
		t.Error("first submission reported as existing")
	}

	second, existing, err := h.queue.Submit(ctx, h.task("req-1", "main", protocol.TaskPush))
	if err != nil {
		t.Fatalf("Submit again: %v", err)
	}
	if !existing {
		t.Error("a repeated request id must report the task as existing, not create a second one")
	}
	if second.ID != first.ID {
		t.Errorf("got task %s, want the original %s", second.ID, first.ID)
	}
}

func TestPartitionKey(t *testing.T) {
	if got := queue.PartitionKey(protocol.TaskPush, "repo", "main"); got != "repo:main" {
		t.Errorf("push key = %q, want %q", got, "repo:main")
	}
	if got := queue.PartitionKey(protocol.TaskPull, "repo", "main"); got != "" {
		t.Errorf("pull key = %q, want empty: pulls are read-only and must not serialize", got)
	}
}

func TestRunnerCompletesTask(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHarness(t, queue.Options{
		LeaseFor:       time.Minute,
		HeartbeatEvery: time.Hour, // never fires during the test
		PollEvery:      time.Millisecond,
	})

	submitted, _, err := h.queue.Submit(ctx, h.task("req-1", "main", protocol.TaskPush))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	done := make(chan struct{})

	runner := queue.NewRunner(h.queue, "worker-1", func(_ context.Context, task *store.Task) ([]byte, error) {
		defer close(done)
		return []byte(`{"upstream_commit":"abc123"}`), nil
	}, quietLogger())

	go runner.Run(ctx)

	waitFor(t, done)

	final := waitForState(t, h.store, submitted.ID, protocol.TaskSucceeded)

	if string(final.Result) != `{"upstream_commit":"abc123"}` {
		t.Errorf("Result = %q", final.Result)
	}
}

func TestRunnerRetriesThenFailsPermanently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHarness(t, queue.Options{
		LeaseFor:       time.Minute,
		HeartbeatEvery: time.Hour,
		PollEvery:      time.Millisecond,
		MaxAttempts:    3,
	})

	submitted, _, err := h.queue.Submit(ctx, h.task("req-1", "main", protocol.TaskPush))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	var mu sync.Mutex
	attempts := 0

	runner := queue.NewRunner(h.queue, "worker-1", func(_ context.Context, task *store.Task) ([]byte, error) {
		mu.Lock()
		attempts++
		mu.Unlock()

		return nil, errors.New("transient clone failure")
	}, quietLogger())

	go runner.Run(ctx)

	final := waitForState(t, h.store, submitted.ID, protocol.TaskFailed)

	mu.Lock()
	got := attempts
	mu.Unlock()

	if got != 3 {
		t.Errorf("handler ran %d times, want MaxAttempts=3", got)
	}
	if final.Error == nil || final.Error.Code != "internal" {
		t.Errorf("Error = %v, want the failure recorded", final.Error)
	}
}

// A denial will be a denial again on the next attempt. Retrying it wastes a
// clone and delays the answer the developer is waiting for.
func TestRunnerDoesNotRetryPermanentFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHarness(t, queue.Options{
		LeaseFor:       time.Minute,
		HeartbeatEvery: time.Hour,
		PollEvery:      time.Millisecond,
		MaxAttempts:    5,
	})

	submitted, _, err := h.queue.Submit(ctx, h.task("req-1", "main", protocol.TaskPush))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	var mu sync.Mutex
	attempts := 0

	runner := queue.NewRunner(h.queue, "worker-1", func(_ context.Context, task *store.Task) ([]byte, error) {
		mu.Lock()
		attempts++
		mu.Unlock()

		return nil, queue.Permanent(protocol.CodeUnauthorizedPaths, "secrets/prod.env")
	}, quietLogger())

	go runner.Run(ctx)

	final := waitForState(t, h.store, submitted.ID, protocol.TaskFailed)

	mu.Lock()
	got := attempts
	mu.Unlock()

	if got != 1 {
		t.Errorf("handler ran %d times, want 1: a permanent failure must not be retried", got)
	}
	if final.Error == nil || final.Error.Code != protocol.CodeUnauthorizedPaths {
		t.Errorf("Error = %v, want the protocol code preserved", final.Error)
	}
}

// The safety property that makes leases usable: a worker whose lease lapsed
// learns about it and stops, instead of pushing work another worker owns.
func TestLeaseLossCancelsHandleContext(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t, queue.Options{
		LeaseFor:       30 * time.Second,
		HeartbeatEvery: 2 * time.Millisecond,
		PollEvery:      time.Millisecond,
	})

	if _, _, err := h.queue.Submit(ctx, h.task("req-1", "main", protocol.TaskPush)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	handle, err := h.queue.Claim(ctx, "worker-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// The worker stalls; its lease expires and another worker takes the task.
	h.clock.Advance(time.Minute)

	if _, err := h.store.Tasks().ReleaseExpired(ctx, h.clock.Now()); err != nil {
		t.Fatalf("ReleaseExpired: %v", err)
	}
	if _, err := h.queue.Claim(ctx, "worker-2"); err != nil {
		t.Fatalf("second Claim: %v", err)
	}

	select {
	case <-handle.Context().Done():
		if cause := context.Cause(handle.Context()); !errors.Is(cause, queue.ErrLeaseLost) {
			t.Errorf("cause = %v, want ErrLeaseLost", cause)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handle context was not cancelled after the lease was lost")
	}
}

// The complement: while a worker heartbeats, its lease must not lapse however
// long the task takes.
func TestHeartbeatKeepsLeaseAliveDuringLongTask(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t, queue.Options{
		LeaseFor:       30 * time.Second,
		HeartbeatEvery: 2 * time.Millisecond,
		PollEvery:      time.Millisecond,
	})

	if _, _, err := h.queue.Submit(ctx, h.task("req-1", "main", protocol.TaskPush)); err != nil {
		t.Fatalf("Submit: %v", err)
	}

	handle, err := h.queue.Claim(ctx, "worker-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Simulate a long clone: time passes well beyond one lease period, but the
	// heartbeat keeps extending it.
	for range 10 {
		h.clock.Advance(10 * time.Second)
		time.Sleep(5 * time.Millisecond)
	}

	released, err := h.store.Tasks().ReleaseExpired(ctx, h.clock.Now())
	if err != nil {
		t.Fatalf("ReleaseExpired: %v", err)
	}
	if released != 0 {
		t.Errorf("released %d tasks; the heartbeat should have kept the lease alive", released)
	}

	select {
	case <-handle.Context().Done():
		t.Fatalf("handle cancelled: %v", context.Cause(handle.Context()))
	default:
	}

	if err := handle.Complete(ctx, []byte(`{}`)); err != nil {
		t.Errorf("Complete: %v", err)
	}
}

// Two runners, one branch: the second must never be handed the same partition
// while the first is working.
func TestPartitionSerializationAcrossRunners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHarness(t, queue.Options{
		LeaseFor:       time.Minute,
		HeartbeatEvery: time.Hour,
		PollEvery:      time.Millisecond,
	})

	for _, id := range []string{"req-1", "req-2", "req-3"} {
		task := h.task(id, "main", protocol.TaskPush)
		if _, _, err := h.queue.Submit(ctx, task); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		h.clock.Advance(time.Second)
	}

	var (
		mu       sync.Mutex
		inFlight int
		maxSeen  int
		finished int
	)

	done := make(chan struct{})

	handler := func(_ context.Context, task *store.Task) ([]byte, error) {
		mu.Lock()
		inFlight++
		if inFlight > maxSeen {
			maxSeen = inFlight
		}
		mu.Unlock()

		time.Sleep(5 * time.Millisecond)

		mu.Lock()
		inFlight--
		finished++
		if finished == 3 {
			close(done)
		}
		mu.Unlock()

		return []byte(`{}`), nil
	}

	for _, holder := range []string{"worker-1", "worker-2", "worker-3"} {
		go queue.NewRunner(h.queue, holder, handler, quietLogger()).Run(ctx)
	}

	waitFor(t, done)

	mu.Lock()
	defer mu.Unlock()

	if maxSeen != 1 {
		t.Errorf("%d tasks ran concurrently on one branch, want 1", maxSeen)
	}
}

// Pull tasks carry no partition key and must not serialize against each other.
func TestPullsRunConcurrently(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	h := newHarness(t, queue.Options{
		LeaseFor:       time.Minute,
		HeartbeatEvery: time.Hour,
		PollEvery:      time.Millisecond,
	})

	for _, id := range []string{"req-1", "req-2"} {
		if _, _, err := h.queue.Submit(ctx, h.task(id, "main", protocol.TaskPull)); err != nil {
			t.Fatalf("Submit: %v", err)
		}
		h.clock.Advance(time.Second)
	}

	started := make(chan struct{}, 2)
	release := make(chan struct{})

	handler := func(_ context.Context, task *store.Task) ([]byte, error) {
		started <- struct{}{}
		<-release
		return []byte(`{}`), nil
	}

	for _, holder := range []string{"worker-1", "worker-2"} {
		go queue.NewRunner(h.queue, holder, handler, quietLogger()).Run(ctx)
	}

	deadline := time.After(2 * time.Second)

	for range 2 {
		select {
		case <-started:
		case <-deadline:
			close(release)
			t.Fatal("pull tasks did not run concurrently")
		}
	}

	close(release)
}

func TestQueuePosition(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t, queue.Options{})

	first, _, err := h.queue.Submit(ctx, h.task("req-1", "main", protocol.TaskPush))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	h.clock.Advance(time.Second)

	second, _, err := h.queue.Submit(ctx, h.task("req-2", "main", protocol.TaskPush))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}

	if pos, err := h.queue.Position(ctx, first.ID); err != nil || pos != 0 {
		t.Errorf("position of first = %d (%v), want 0", pos, err)
	}
	if pos, err := h.queue.Position(ctx, second.ID); err != nil || pos != 1 {
		t.Errorf("position of second = %d (%v), want 1", pos, err)
	}
}

func TestReaperReleasesStrandedTasks(t *testing.T) {
	ctx := context.Background()

	h := newHarness(t, queue.Options{
		LeaseFor:       30 * time.Second,
		HeartbeatEvery: time.Hour,
	})

	if _, _, err := h.queue.Submit(ctx, h.task("req-1", "main", protocol.TaskPush)); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	if _, err := h.queue.Claim(ctx, "doomed-worker"); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	h.clock.Advance(time.Minute)

	released, err := h.queue.ReapExpired(ctx)
	if err != nil {
		t.Fatalf("ReapExpired: %v", err)
	}
	if released != 1 {
		t.Errorf("released %d tasks, want 1", released)
	}
}

// ---------------------------------------------------------------------------

func waitFor(t *testing.T, done <-chan struct{}) {
	t.Helper()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for the handler")
	}
}

func waitForState(t *testing.T, s *memory.Store, id store.ID, want protocol.TaskState) *store.Task {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		task, err := s.Tasks().ByID(context.Background(), id)
		if err != nil {
			t.Fatalf("ByID: %v", err)
		}
		if task.State == want {
			return task
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("task %s did not reach state %s", id, want)
	return nil
}
