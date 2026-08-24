package auditbuf_test

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/auditbuf"
	"github.com/NitScm/nit/pkg/audit"
	"github.com/NitScm/nit/pkg/audit/audittest"
)

func quiet() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// A buffer is a sink like any other, and the reason it exists — sitting between
// the server and a destination — is exactly the reason it must not change what
// arrives. Held to the same suite as the thing it wraps.
func TestConformance(t *testing.T) {
	audittest.Run(t, func(t *testing.T) (audit.Sink, audittest.Readback) {
		destination := audit.NewMemory()

		buffer := auditbuf.New(destination, auditbuf.Options{
			FlushEvery: time.Millisecond,
			Log:        quiet(),
		})

		t.Cleanup(func() { buffer.Close() })

		return buffer, func(t *testing.T, want int) []audit.Record {
			return audittest.Poll(t, want, destination.Records)
		}
	})
}

// The property the package exists for. A destination that never returns must
// cost a push nothing.
func TestEmitDoesNotWaitForTheDestination(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	buffer := auditbuf.New(blocking{release: release}, auditbuf.Options{
		Capacity:   8,
		BatchSize:  1,
		FlushEvery: time.Millisecond,
		Log:        quiet(),
	})

	done := make(chan struct{})

	go func() {
		defer close(done)

		for range 8 {
			_ = buffer.Emit(context.Background(), audit.Record{Action: "push.accepted"})
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Emit blocked on a destination that never returns")
	}
}

// A developer pressing Ctrl-C cancels their push. It must not also cancel the
// record of what was decided before they did.
func TestTheCallersCancellationDoesNotReachTheDestination(t *testing.T) {
	destination := audit.NewMemory()

	buffer := auditbuf.New(&contextRecorder{inner: destination}, auditbuf.Options{
		FlushEvery: time.Millisecond,
		Log:        quiet(),
	})
	defer buffer.Close()

	ctx, cancel := context.WithCancel(context.Background())

	if err := buffer.Emit(ctx, audit.Record{Action: "push.accepted"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	cancel()

	if got := audittest.Poll(t, 1, destination.Records); len(got) != 1 {
		t.Fatalf("received %d records, want 1: cancelling the request lost the decision", len(got))
	}
}

// Records accepted before Close are records this promised to carry.
func TestCloseFlushesWhatWasAccepted(t *testing.T) {
	destination := audit.NewMemory()

	// A flush interval long enough that nothing would leave on its own, so a
	// pass here is Close doing the work rather than the ticker.
	buffer := auditbuf.New(destination, auditbuf.Options{
		FlushEvery: time.Hour,
		Log:        quiet(),
	})

	const count = 20

	for i := range count {
		if err := buffer.Emit(context.Background(), audit.Record{
			Action:     "push.accepted",
			RequestID:  string(rune('a' + i)),
			Repository: "payments",
		}); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}

	if err := buffer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := destination.Len(); got != count {
		t.Errorf("the destination has %d records, want %d: Close did not flush", got, count)
	}
}

func TestCloseTwiceIsSafe(t *testing.T) {
	buffer := auditbuf.New(audit.NewMemory(), auditbuf.Options{Log: quiet()})

	if err := buffer.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}

	if err := buffer.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// A full queue drops rather than blocks, because the database still holds every
// record. What must not happen is dropping silently: an export gap nobody can
// see is an audit trail nobody can trust.
func TestAFullQueueDropsAndCounts(t *testing.T) {
	release := make(chan struct{})

	buffer := auditbuf.New(blocking{release: release}, auditbuf.Options{
		Capacity:   4,
		BatchSize:  1,
		FlushEvery: time.Millisecond,
		Log:        quiet(),
	})

	// Far more than the queue holds, against a destination that never drains.
	for range 200 {
		_ = buffer.Emit(context.Background(), audit.Record{Action: "push.accepted"})
	}

	if buffer.Dropped() == 0 {
		t.Error("a queue of 4 swallowed 200 records without counting a drop")
	}

	close(release)
	buffer.Close()
}

// A destination that fails every attempt must not retry forever, and what it
// gives up on must be counted.
func TestAFailingDestinationIsGivenUpOnAndCounted(t *testing.T) {
	destination := audit.NewMemory()
	destination.Fail(errors.New("unreachable"))

	buffer := auditbuf.New(destination, auditbuf.Options{
		BatchSize:  1,
		FlushEvery: time.Millisecond,
		Retries:    2,
		Backoff:    time.Millisecond,
		Log:        quiet(),
	})

	if err := buffer.Emit(context.Background(), audit.Record{Action: "push.accepted"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for buffer.Dropped() == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	if buffer.Dropped() != 1 {
		t.Errorf("Dropped = %d, want 1: a batch nobody could deliver was not counted",
			buffer.Dropped())
	}

	buffer.Close()
}

// A destination that fails once and then recovers must not lose the batch: an
// export that gave up on the first refused connection would be an export that
// never survives a restart of the thing it exports to.
func TestATransientFailureIsRetried(t *testing.T) {
	destination := audit.NewMemory()
	destination.Fail(errors.New("connection refused"))

	buffer := auditbuf.New(destination, auditbuf.Options{
		BatchSize:  1,
		FlushEvery: time.Millisecond,
		Retries:    5,
		Backoff:    time.Millisecond,
		Log:        quiet(),
	})
	defer buffer.Close()

	if err := buffer.Emit(context.Background(), audit.Record{Action: "push.accepted"}); err != nil {
		t.Fatalf("Emit: %v", err)
	}

	time.Sleep(5 * time.Millisecond)
	destination.Fail(nil)

	if got := audittest.Poll(t, 1, destination.Records); len(got) != 1 {
		t.Fatalf("received %d records, want 1: the batch was not re-offered", len(got))
	}

	if buffer.Dropped() != 0 {
		t.Errorf("Dropped = %d, want 0: a batch that eventually landed was counted lost",
			buffer.Dropped())
	}
}

// Nothing consumes the queue after Close, so a record accepted then would be
// lost without being counted — which is the one kind of loss this package
// exists to make impossible.
func TestEmitAfterCloseIsCountedRatherThanSwallowed(t *testing.T) {
	destination := audit.NewMemory()

	buffer := auditbuf.New(destination, auditbuf.Options{Log: quiet()})

	if err := buffer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if err := buffer.Emit(context.Background(), audit.Record{Action: "push.accepted"}); err != nil {
		t.Fatalf("Emit after Close: %v", err)
	}

	if buffer.Dropped() != 1 {
		t.Errorf("Dropped = %d, want 1", buffer.Dropped())
	}

	if destination.Len() != 0 {
		t.Errorf("the destination received %d records after Close", destination.Len())
	}
}

// Wrapping unconditionally is what lets pkg/nitd stay simple: "no export
// configured" must stay the absence of a sink, not an idle goroutine.
func TestWrappingNothingIsNothing(t *testing.T) {
	buffer := auditbuf.New(nil, auditbuf.Options{Log: quiet()})

	if buffer != nil {
		t.Fatal("New(nil) returned a buffer, so a deployment with no export runs a goroutine for it")
	}

	// The nil buffer is still usable as a sink, which is what makes the
	// unconditional wrap safe.
	if err := buffer.Emit(context.Background(), audit.Record{Action: "push.accepted"}); err != nil {
		t.Errorf("Emit on a nil buffer: %v", err)
	}

	if err := buffer.Close(); err != nil {
		t.Errorf("Close on a nil buffer: %v", err)
	}
}

// ---------------------------------------------------------------------------
// doubles
// ---------------------------------------------------------------------------

// blocking is a destination that never returns until released.
type blocking struct{ release chan struct{} }

func (b blocking) Emit(ctx context.Context, _ ...audit.Record) error {
	select {
	case <-b.release:
	case <-ctx.Done():
	}

	return nil
}

// contextRecorder fails the emit if the context it is handed is already done,
// which is how the caller's cancellation would show up here.
type contextRecorder struct {
	inner audit.Sink

	mu sync.Mutex
}

func (c *contextRecorder) Emit(ctx context.Context, records ...audit.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}

	return c.inner.Emit(ctx, records...)
}

// retaining is the sink the suite forbids: it keeps the slice it was handed
// instead of copying. It exists to prove the buffer does not depend on every
// sink having read the suite — reusing the batch array would rewrite what a
// sink like this kept, and the corruption would look like records attributed
// to the wrong person.
type retaining struct {
	mu    sync.Mutex
	kept  [][]audit.Record
	count int
}

func (r *retaining) Emit(_ context.Context, records ...audit.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.kept = append(r.kept, records)
	r.count += len(records)

	return nil
}

func (r *retaining) batches() [][]audit.Record {
	r.mu.Lock()
	defer r.mu.Unlock()

	return append([][]audit.Record(nil), r.kept...)
}

func (r *retaining) delivered() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.count
}

func TestASinkThatKeptItsBatchIsNotOverwritten(t *testing.T) {
	destination := &retaining{}

	buffer := auditbuf.New(destination, auditbuf.Options{
		BatchSize:  1,
		FlushEvery: time.Millisecond,
		Log:        quiet(),
	})

	for _, actor := range []string{"maya", "sam", "kim"} {
		if err := buffer.Emit(context.Background(), audit.Record{
			Actor:  actor,
			Action: "push.accepted",
		}); err != nil {
			t.Fatalf("Emit: %v", err)
		}
	}

	if err := buffer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if got := destination.delivered(); got != 3 {
		t.Fatalf("delivered %d records, want 3", got)
	}

	var actors []string
	for _, batch := range destination.batches() {
		for _, record := range batch {
			actors = append(actors, record.Actor)
		}
	}

	want := []string{"maya", "sam", "kim"}
	for i, actor := range want {
		if actors[i] != actor {
			t.Errorf("the sink now holds %q at %d, want %q: the buffer reused the "+
				"array it had already handed over\n%v", actors[i], i, actor, actors)
		}
	}
}
