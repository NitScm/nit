// Package auditbuf keeps a slow audit destination off the request path.
//
// pkg/audit tells a Sink implementation that "an implementation that talks to a
// network buffers and returns". That is the right rule and the wrong place to
// leave it: it made every sink author write the same queue, and a queue written
// once per destination is a queue that loses records in a different way per
// destination. This is that queue, written once, so a sink can be the thing it
// should be — a client that speaks one protocol.
//
// # What it guarantees
//
//   - Emit never blocks. It hands the records to a background goroutine and
//     returns, so a destination that is down or slow costs a push nothing.
//
//   - Delivery does not use the caller's context. A developer pressing Ctrl-C
//     cancels their push; it must not also cancel the record of what was
//     decided before they did.
//
// # What it does not guarantee
//
// Delivery. The queue is bounded, and a destination that stays unreachable
// fills it; past that, records are dropped and counted rather than held at the
// cost of memory the server needs for work it can still do.
//
// That is a deliberate limit and not a hole, because a sink is never the only
// copy: every record was written to the database first, and the database is the
// record of truth. Recovering an export gap is a matter of replaying from it —
// which is a thing to build, not a thing to pretend a bigger buffer solves.
package auditbuf

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NitScm/nit/pkg/audit"
)

// Defaults chosen so an operator who configures nothing gets sane behaviour.
const (
	// DefaultCapacity is roughly a minute of a busy deployment's decisions —
	// enough to ride out a destination restart, small enough that a permanent
	// outage costs bounded memory.
	DefaultCapacity = 4096

	// DefaultBatchSize is what a single Emit carries. Larger amortises the
	// round trip; too large turns one failure into a large loss.
	DefaultBatchSize = 256

	// DefaultFlushEvery bounds how long a record waits behind an idle queue.
	// An audit trail a minute behind is fine; one that waits for a batch to
	// fill can be hours behind on a quiet repository.
	DefaultFlushEvery = time.Second

	// DefaultTimeout bounds one attempt at the destination.
	DefaultTimeout = 30 * time.Second

	// DefaultRetries is how many times a batch is re-offered before it is
	// dropped. The database still holds it.
	DefaultRetries = 3

	// DefaultBackoff is the pause before the first retry; it doubles, capped.
	DefaultBackoff = 250 * time.Millisecond
)

// Options configures a Buffer. The zero value is usable: every field falls back
// to the default above.
type Options struct {
	Capacity   int
	BatchSize  int
	FlushEvery time.Duration
	Timeout    time.Duration
	Retries    int
	Backoff    time.Duration
	Log        *slog.Logger
}

// Buffer is an audit.Sink that forwards to another one, asynchronously.
type Buffer struct {
	sink audit.Sink
	opts Options
	log  *slog.Logger

	in     chan audit.Record
	closed chan struct{}
	done   chan struct{}

	once    sync.Once
	dropped atomic.Uint64
}

// New starts a buffer in front of sink. Close it to flush.
//
// A nil sink returns nil, so a caller can wrap unconditionally: "no export
// configured" stays the absence of a sink rather than an empty queue with a
// goroutine behind it.
func New(sink audit.Sink, opts Options) *Buffer {
	if sink == nil {
		return nil
	}

	if opts.Capacity <= 0 {
		opts.Capacity = DefaultCapacity
	}
	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultBatchSize
	}
	if opts.FlushEvery <= 0 {
		opts.FlushEvery = DefaultFlushEvery
	}
	if opts.Timeout <= 0 {
		opts.Timeout = DefaultTimeout
	}
	if opts.Retries < 0 {
		opts.Retries = DefaultRetries
	}
	if opts.Backoff <= 0 {
		opts.Backoff = DefaultBackoff
	}

	log := opts.Log
	if log == nil {
		log = slog.Default()
	}

	b := &Buffer{
		sink:   sink,
		opts:   opts,
		log:    log,
		in:     make(chan audit.Record, opts.Capacity),
		closed: make(chan struct{}),
		done:   make(chan struct{}),
	}

	go b.run()

	return b
}

// Emit implements audit.Sink. It never blocks and never returns an error.
//
// Not returning an error is the honest signature: the caller is on a request
// path and has already been told not to fail the operation over an audit write,
// so an error here would only ever be logged — and this package can log it with
// the context of *why* it happened, which the caller does not have.
func (b *Buffer) Emit(_ context.Context, records ...audit.Record) error {
	if b == nil || len(records) == 0 {
		return nil
	}

	// After Close nothing consumes the queue, so accepting a record here would
	// lose it without counting it. Refusing it counts it instead.
	select {
	case <-b.closed:
		b.dropped.Add(uint64(len(records)))

		return nil
	default:
	}

	for _, record := range records {
		select {
		case b.in <- record:
		default:
			// Full. Dropping beats blocking a push, and beats growing without
			// bound: the database has this record either way.
			if n := b.dropped.Add(1); n == 1 || n%1000 == 0 {
				b.log.Warn("audit export queue full, records dropped",
					"dropped_total", n, "capacity", b.opts.Capacity,
					"note", "the database still holds every record")
			}
		}
	}

	return nil
}

// Dropped reports how many records the queue refused, for a metric or a
// shutdown line. A deployment where this is not zero has an export gap and
// should know it without reading logs.
func (b *Buffer) Dropped() uint64 {
	if b == nil {
		return 0
	}

	return b.dropped.Load()
}

// Close stops accepting records and flushes what is queued.
//
// It waits for the drain, so a shutdown that gives this a moment loses nothing
// that had already been accepted. Calling it twice is safe.
//
// The wait is bounded by one attempt at the destination — Options.Timeout —
// and no retry: a process holding itself open to re-offer a batch to a sink
// that is already failing has chosen the wrong thing to protect.
func (b *Buffer) Close() error {
	if b == nil {
		return nil
	}

	b.once.Do(func() { close(b.closed) })

	<-b.done

	if n := b.dropped.Load(); n > 0 {
		b.log.Warn("audit export finished with a gap", "dropped_total", n)
	}

	return nil
}

// run is the single consumer, which is also what keeps a batch in the order the
// policy produced it.
func (b *Buffer) run() {
	defer close(b.done)

	ticker := time.NewTicker(b.opts.FlushEvery)
	defer ticker.Stop()

	batch := make([]audit.Record, 0, b.opts.BatchSize)

	flush := func() {
		if len(batch) == 0 {
			return
		}

		b.deliver(batch)

		// A fresh slice rather than batch[:0]: deliver may hand the records to
		// a sink that keeps them, and reusing the array would rewrite what it
		// kept. The suite forbids a sink doing that; a buffer that relies on
		// every sink having read the suite is a buffer waiting for one that
		// did not.
		batch = make([]audit.Record, 0, b.opts.BatchSize)
	}

	for {
		select {
		case record := <-b.in:
			batch = append(batch, record)

			if len(batch) >= b.opts.BatchSize {
				flush()
			}

		case <-ticker.C:
			flush()

		case <-b.closed:
			// Drain what is queued, then flush the remainder. Records accepted
			// before Close was called are records this promised to carry.
			for {
				select {
				case record := <-b.in:
					batch = append(batch, record)

					if len(batch) >= b.opts.BatchSize {
						flush()
					}

					continue
				default:
				}

				break
			}

			flush()

			return
		}
	}
}

// deliver offers a batch to the sink, retrying a bounded number of times.
func (b *Buffer) deliver(batch []audit.Record) {
	backoff := b.opts.Backoff

	for attempt := 0; ; attempt++ {
		// A fresh context every attempt, derived from nothing: the request that
		// produced these records is long gone, and may have been cancelled by
		// the developer who made it.
		ctx, cancel := context.WithTimeout(context.Background(), b.opts.Timeout)
		err := b.sink.Emit(ctx, batch...)

		cancel()

		if err == nil {
			return
		}

		if attempt >= b.opts.Retries {
			b.dropped.Add(uint64(len(batch)))

			b.log.Error("audit export failed, records dropped",
				"error", err, "records", len(batch), "attempts", attempt+1,
				"note", "the database still holds every record")

			return
		}

		select {
		case <-time.After(backoff):
		case <-b.closed:
			// Shutting down. Sitting out a backoff to retry a destination that
			// is already failing trades a clean stop for records the database
			// already holds.
			b.dropped.Add(uint64(len(batch)))

			b.log.Error("audit export abandoned at shutdown",
				"error", err, "records", len(batch),
				"note", "the database still holds every record")

			return
		}

		if backoff < 8*b.opts.Backoff {
			backoff *= 2
		}
	}
}

var _ audit.Sink = (*Buffer)(nil)
