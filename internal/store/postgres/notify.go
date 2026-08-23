package postgres

import (
	"context"
	"time"

	"github.com/NitScm/nit/pkg/store"
)

// taskChannel is what migration 0003's trigger notifies on.
const taskChannel = "nit_task_changed"

// WatchTasks delivers the id of each task whose state changed.
//
// One connection is held for the life of the watch, taken out of the pool and
// kept — LISTEN is a property of a session, so a notification only arrives on
// the connection that registered for it. That is one connection per nitd
// process, not per waiting client, which is the difference between this being
// worth doing and being worse than the poll it replaces.
//
// A dropped notification is not an error to report. The connection can fail
// and reconnect across a change, and this reconnects rather than giving up:
// the caller's poll is what guarantees liveness, and this only shortens the
// wait. See store.TaskNotifier.
func (s *Store) WatchTasks(ctx context.Context) (<-chan store.ID, error) {
	// Buffered, so a burst of completions does not block the listener while a
	// consumer is busy. Past the buffer the send is dropped, which the "hint,
	// never a substitute" contract is what makes safe.
	out := make(chan store.ID, 256)

	go func() {
		defer close(out)

		for ctx.Err() == nil {
			if err := s.listen(ctx, out); err != nil && ctx.Err() == nil {
				// Reconnect rather than stop. A control plane that lost its
				// listener at the first network blip would silently fall back
				// to poll-only latency for the rest of its life, with nothing
				// saying so.
				select {
				case <-ctx.Done():
				case <-time.After(time.Second):
				}
			}
		}
	}()

	return out, nil
}

// listen holds one connection and forwards notifications until it fails.
func (s *Store) listen(ctx context.Context, out chan<- store.ID) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "LISTEN "+taskChannel); err != nil {
		return err
	}

	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			return err
		}

		select {
		case out <- store.ID(notification.Payload):
		default:
			// The consumer is behind. Dropping is correct: it re-reads the row
			// it cares about anyway, and blocking here would stall every other
			// waiter behind one slow one.
		}
	}
}

var _ store.TaskNotifier = (*Store)(nil)
