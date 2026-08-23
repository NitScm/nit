// Package taskevents wakes waiting clients when their task moves.
//
// `GET /v1/tasks/{id}/events` holds a long poll open while a developer watches
// their own push. Without this it re-reads the row every 500 ms per waiter: one
// query per developer per half second, for a row that changes twice in a task's
// life, and a wait that is felt because somebody is looking at it.
//
// # What this is, and what it deliberately is not
//
// It is a hint that shortens the wait. It is **not** the thing that guarantees
// a client is ever woken — the poll still is, at a much lower rate.
//
// That distinction is the whole design. A notification can be dropped: the
// listening connection can fail and reconnect across a change, a slow consumer
// can overflow a buffer, and a backend may have no notification mechanism at
// all (MySQL and MariaDB have none). A hub that a caller trusted for liveness
// would hang a developer's push forever the first time any of those happened.
// So Wait takes both a notification channel and a deadline, and either is
// allowed to be the one that returns.
package taskevents

import (
	"context"
	"sync"

	"github.com/NitScm/nit/pkg/store"
)

// Hub fans notifications out to the waiters interested in each task.
//
// The zero value is usable and notifies nobody, which is exactly what a backend
// without a notifier should get: Wait then returns only on its deadline, and
// the caller's poll is unchanged.
type Hub struct {
	mu      sync.Mutex
	waiters map[store.ID]map[chan struct{}]struct{}
}

// New returns an empty hub.
func New() *Hub {
	return &Hub{waiters: map[store.ID]map[chan struct{}]struct{}{}}
}

// Run forwards notifications from a store until ctx is done.
//
// A store that does not implement store.TaskNotifier is not an error: Run
// reports that it did nothing and returns, and every waiter falls back to its
// deadline.
func (h *Hub) Run(ctx context.Context, s store.Store) (started bool, err error) {
	notifier, ok := s.(store.TaskNotifier)
	if !ok {
		return false, nil
	}

	changes, err := notifier.WatchTasks(ctx)
	if err != nil {
		return false, err
	}

	go func() {
		for id := range changes {
			h.notify(id)
		}
	}()

	return true, nil
}

// Subscribe registers interest in a task. Close it when done.
//
// Subscription and waiting are separate on purpose, and the order is the point:
// **subscribe before reading the task, then wait.** A caller that read first
// and subscribed second would miss a change landing in between and sit on its
// poll for no reason — which is the latency this package exists to remove,
// reintroduced in the one place nobody would look for it.
func (h *Hub) Subscribe(id store.ID) *Subscription {
	// Buffered, so a notification arriving before the caller waits is kept
	// rather than dropped. That is what makes "subscribe, then read, then
	// wait" safe.
	ch := make(chan struct{}, 1)

	h.mu.Lock()
	defer h.mu.Unlock()

	if h.waiters == nil {
		h.waiters = map[store.ID]map[chan struct{}]struct{}{}
	}
	if h.waiters[id] == nil {
		h.waiters[id] = map[chan struct{}]struct{}{}
	}

	h.waiters[id][ch] = struct{}{}

	return &Subscription{hub: h, id: id, ch: ch}
}

// Subscription is one caller's interest in one task.
type Subscription struct {
	hub *Hub
	id  store.ID
	ch  chan struct{}
}

// Wait blocks until the task is reported changed, the context is done, or the
// caller's own signal fires — whichever comes first.
//
// tick is the caller's poll. Passing it in rather than owning it here is what
// keeps the poll the liveness guarantee: this package cannot accidentally
// become the only thing that wakes anybody.
func (s *Subscription) Wait(ctx context.Context, tick <-chan struct{}) {
	if s == nil {
		<-ctx.Done()
		return
	}

	select {
	case <-ctx.Done():
	case <-s.ch:
	case <-tick:
	}
}

// Close forgets the subscription.
func (s *Subscription) Close() {
	if s == nil {
		return
	}

	s.hub.unsubscribe(s.id, s.ch)
}

func (h *Hub) unsubscribe(id store.ID, ch chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if set := h.waiters[id]; set != nil {
		delete(set, ch)

		// The map is keyed by task id and a control plane sees a great many
		// tasks. Leaving an empty set behind for each would be a leak that only
		// shows up after a week of uptime.
		if len(set) == 0 {
			delete(h.waiters, id)
		}
	}
}

func (h *Hub) notify(id store.ID) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for ch := range h.waiters[id] {
		select {
		case ch <- struct{}{}:
		default:
			// Already signalled and not yet read. One wake-up is all a waiter
			// needs: it re-reads the task, which is where the truth is.
		}
	}
}

// Waiting reports how many waiters are registered, for tests and diagnostics.
func (h *Hub) Waiting() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	total := 0
	for _, set := range h.waiters {
		total += len(set)
	}

	return total
}
