package taskevents_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/store/memory"
	"github.com/NitScm/nit/internal/taskevents"
	"github.com/NitScm/nit/pkg/store"
)

// The zero value has to work, because that is what a backend with no
// notification mechanism gets. A hub that panicked or blocked there would take
// the MySQL deployment down rather than making it a little slower.
func TestTheZeroValueNotifiesNobody(t *testing.T) {
	var hub taskevents.Hub

	tick := make(chan struct{}, 1)
	tick <- struct{}{}

	done := make(chan struct{})

	go func() {
		sub := hub.Subscribe("task-1")
		defer sub.Close()
		sub.Wait(context.Background(), tick)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return on the caller's own signal")
	}
}

// A hub with no notifier still has to return on the caller's poll: the poll is
// the liveness guarantee, and a waiter that only listened for notifications
// would hang for good on a backend that has none.
func TestWaitReturnsOnTheCallersSignal(t *testing.T) {
	hub := taskevents.New()

	tick := make(chan struct{}, 1)

	go func() {
		time.Sleep(20 * time.Millisecond)
		tick <- struct{}{}
	}()

	start := time.Now()
	sub := hub.Subscribe("task-1")
	defer sub.Close()
	sub.Wait(context.Background(), tick)

	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("Wait took %s", elapsed)
	}
}

func TestANotificationWakesTheWaiter(t *testing.T) {
	hub := taskevents.New()

	changes := make(chan store.ID, 1)
	notifier := &fakeNotifier{changes: changes}

	started, err := hub.Run(context.Background(), notifier)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !started {
		t.Fatal("Run did not start for a store that implements TaskNotifier")
	}

	// No tick at all: only the notification can end this wait, which is what
	// proves the notification is doing the work.
	never := make(chan struct{})
	done := make(chan struct{})

	sub := hub.Subscribe("task-1")
	defer sub.Close()

	go func() {
		sub.Wait(context.Background(), never)
		close(done)
	}()

	waitForWaiters(t, hub, 1)

	changes <- "task-1"

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a notification did not wake the waiter")
	}
}

// A notification for one task must not wake a waiter on another: a control
// plane has many in flight, and waking every waiter for every change would put
// the poll's load back with extra steps.
func TestANotificationWakesOnlyItsOwnWaiter(t *testing.T) {
	hub := taskevents.New()

	changes := make(chan store.ID, 1)

	if _, err := hub.Run(context.Background(), &fakeNotifier{changes: changes}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	never := make(chan struct{})
	other := make(chan struct{})

	otherSub := hub.Subscribe("task-other")
	defer otherSub.Close()

	go func() {
		otherSub.Wait(context.Background(), never)
		close(other)
	}()

	waitForWaiters(t, hub, 1)

	changes <- "task-1"

	select {
	case <-other:
		t.Fatal("a waiter on another task was woken")
	case <-time.After(200 * time.Millisecond):
	}
}

// A change that lands between subscribing and selecting must not be lost, or a
// task that finished quickly leaves its client on the full deadline for no
// reason.
func TestANotificationIsNotLostInTheGap(t *testing.T) {
	hub := taskevents.New()

	changes := make(chan store.ID, 1)

	if _, err := hub.Run(context.Background(), &fakeNotifier{changes: changes}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	never := make(chan struct{})

	for range 50 {
		done := make(chan struct{})

		// Subscribed first, then the change is sent, then the wait starts —
		// the order a caller must use, and the one that makes this window
		// closable at all.
		sub := hub.Subscribe("task-1")

		changes <- "task-1"

		go func() {
			sub.Wait(context.Background(), never)
			sub.Close()
			close(done)
		}()

		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("a notification racing the subscription was lost")
		}
	}
}

// A waiter that has gone must leave nothing behind. The map is keyed by task
// id and a control plane sees a great many; a leak here shows up after a week
// of uptime, which is the worst time to find one.
func TestWaitersAreForgotten(t *testing.T) {
	hub := taskevents.New()

	tick := make(chan struct{})
	close(tick)

	// Closed inside the loop, not deferred: a deferred Close would run at the
	// end of the test and the assertion below would pass for the wrong reason.
	for range 100 {
		sub := hub.Subscribe("task-1")
		sub.Wait(context.Background(), tick)
		sub.Close()
	}

	if got := hub.Waiting(); got != 0 {
		t.Errorf("%d waiters remain after all of them returned", got)
	}
}

func TestConcurrentWaitersAndNotifications(t *testing.T) {
	hub := taskevents.New()

	changes := make(chan store.ID, 64)

	if _, err := hub.Run(context.Background(), &fakeNotifier{changes: changes}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var wg sync.WaitGroup

	for i := range 32 {
		wg.Add(1)

		go func() {
			defer wg.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			tick := make(chan struct{})

			sub := hub.Subscribe(store.ID(string(rune('a' + i%8))))
			defer sub.Close()

			sub.Wait(ctx, tick)
		}()
	}

	go func() {
		for range 200 {
			for i := range 8 {
				changes <- store.ID(string(rune('a' + i)))
			}
		}
	}()

	wg.Wait()

	if got := hub.Waiting(); got != 0 {
		t.Errorf("%d waiters remain", got)
	}
}

// A store with no notifier is the ordinary case on MySQL, and it must be
// reported as such rather than as a failure.
func TestAStoreWithoutANotifierIsNotAnError(t *testing.T) {
	hub := taskevents.New()

	started, err := hub.Run(context.Background(), memory.New())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if started {
		t.Error("Run claims to have started for a store that cannot notify")
	}
}

// fakeNotifier is a store that can be told what to announce.
type fakeNotifier struct {
	store.Store

	changes chan store.ID
}

func (f *fakeNotifier) WatchTasks(context.Context) (<-chan store.ID, error) {
	return f.changes, nil
}

func waitForWaiters(t *testing.T, hub *taskevents.Hub, want int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)

	for time.Now().Before(deadline) {
		if hub.Waiting() >= want {
			return
		}

		time.Sleep(time.Millisecond)
	}

	t.Fatalf("only %d waiters registered, want %d", hub.Waiting(), want)
}
