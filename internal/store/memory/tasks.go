package memory

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

type taskStore Store

func (s *taskStore) Create(_ context.Context, t *store.Task) (*store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Idempotency is enforced here rather than in the caller: a duplicate
	// request must never be able to become a second upstream commit, whatever
	// the API layer does.
	if t.RequestID != "" {
		for _, existing := range s.tasks {
			if existing.TenantID == t.TenantID && existing.RequestID == t.RequestID {
				return cloneTask(existing), store.ErrDuplicateRequest
			}
		}
	}

	created := *t
	if created.ID == "" {
		created.ID = (*Store)(s).nextID("task")
	}
	if created.State == "" {
		created.State = protocol.TaskQueued
	}
	if created.CreatedAt.IsZero() {
		created.CreatedAt = time.Now().UTC()
	}
	created.UpdatedAt = created.CreatedAt

	s.tasks[created.ID] = &created

	return cloneTask(&created), nil
}

func (s *taskStore) ByID(_ context.Context, id store.ID) (*store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return nil, store.ErrNotFound
	}

	return cloneTask(t), nil
}

func (s *taskStore) ByRequestID(_ context.Context, tenant policy.TenantID, requestID string) (*store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, t := range s.tasks {
		if t.TenantID == tenant && t.RequestID == requestID {
			return cloneTask(t), nil
		}
	}

	return nil, store.ErrNotFound
}

func (s *taskStore) List(_ context.Context, f store.TaskFilter) ([]*store.Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []*store.Task

	for _, t := range s.tasks {
		switch {
		case f.Tenant != "" && t.TenantID != f.Tenant:
			continue
		case f.UserID != "" && t.UserID != f.UserID:
			continue
		case f.RepositoryID != "" && t.RepositoryID != f.RepositoryID:
			continue
		case f.Branch != "" && t.Branch != f.Branch:
			continue
		case len(f.States) > 0 && !containsState(f.States, t.State):
			continue
		case len(f.Kinds) > 0 && !containsKind(f.Kinds, t.Kind):
			continue
		}

		out = append(out, cloneTask(t))
	}

	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].ID > out[j].ID
	})

	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}

	return out, nil
}

// Claim takes the oldest dispatchable task and leases it.
//
// A task is dispatchable when it is queued, its kind is wanted, and its
// partition has nothing running. Expired leases are reclaimed first, so a
// worker that died mid-task cannot strand its branch: the next claim attempt
// recovers it without any separate reaper having to run.
func (s *taskStore) Claim(_ context.Context, opts store.ClaimOptions) (*store.Task, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	s.releaseExpiredLocked(now)

	busy := s.busyPartitionsLocked()

	candidates := make([]*store.Task, 0, len(s.tasks))

	for _, t := range s.tasks {
		if t.State != protocol.TaskQueued {
			continue
		}
		if len(opts.Kinds) > 0 && !containsKind(opts.Kinds, t.Kind) {
			continue
		}
		if t.PartitionKey != "" && busy[t.PartitionKey] {
			continue
		}

		candidates = append(candidates, t)
	}

	if len(candidates) == 0 {
		return nil, store.ErrNoTask
	}

	// FIFO, so a branch's queue is fair and no developer's push is starved.
	sort.Slice(candidates, func(i, j int) bool {
		if !candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
		}
		return candidates[i].ID < candidates[j].ID
	})

	leaseFor := opts.LeaseFor
	if leaseFor <= 0 {
		leaseFor = time.Minute
	}

	claimed := candidates[0]
	claimed.State = protocol.TaskRunning
	claimed.Attempts++
	claimed.UpdatedAt = now
	claimed.Lease = &store.Lease{
		Holder:    opts.Holder,
		Token:     newToken(),
		ExpiresAt: now.Add(leaseFor),
	}

	if claimed.StartedAt == nil {
		started := now
		claimed.StartedAt = &started
	}

	return cloneTask(claimed), nil
}

func (s *taskStore) Heartbeat(_ context.Context, id store.ID, token string, until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}
	if err := checkLease(t, token); err != nil {
		return err
	}

	t.Lease.ExpiresAt = until
	t.UpdatedAt = until

	return nil
}

func (s *taskStore) Complete(_ context.Context, id store.ID, token string, result []byte, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}
	if err := checkLease(t, token); err != nil {
		return err
	}
	if err := checkFinishOrder(t, at); err != nil {
		return err
	}

	t.State = protocol.TaskSucceeded
	t.Result = append([]byte(nil), result...)
	t.Error = nil
	t.Lease = nil
	t.UpdatedAt = at

	finished := at
	t.FinishedAt = &finished

	return nil
}

func (s *taskStore) Fail(_ context.Context, id store.ID, token string, cause *protocol.Error, requeue bool, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}
	if err := checkLease(t, token); err != nil {
		return err
	}

	t.Error = cause
	t.Lease = nil
	t.UpdatedAt = at

	if requeue {
		// The attempt counter is deliberately not reset: it is what lets a
		// caller give up on a task that fails repeatedly instead of cycling
		// forever through the queue.
		t.State = protocol.TaskQueued
		t.StartedAt = nil
		return nil
	}

	t.State = protocol.TaskFailed

	finished := at
	t.FinishedAt = &finished

	return nil
}

func (s *taskStore) ReleaseExpired(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.releaseExpiredLocked(now), nil
}

func (s *taskStore) QueuePosition(_ context.Context, id store.ID) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return 0, store.ErrNotFound
	}
	if t.State != protocol.TaskQueued {
		return 0, nil
	}

	// An unpartitioned task competes with nobody.
	if t.PartitionKey == "" {
		return 0, nil
	}

	position := 0

	for _, other := range s.tasks {
		if other.ID == t.ID || other.PartitionKey != t.PartitionKey {
			continue
		}

		switch other.State {
		case protocol.TaskRunning:
			position++
		case protocol.TaskQueued:
			if other.CreatedAt.Before(t.CreatedAt) ||
				(other.CreatedAt.Equal(t.CreatedAt) && other.ID < t.ID) {
				position++
			}
		}
	}

	return position, nil
}

func (s *taskStore) Cancel(_ context.Context, id store.ID, at time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	t, ok := s.tasks[id]
	if !ok {
		return store.ErrNotFound
	}

	// A running task belongs to its worker. Cancelling the record underneath it
	// would leave the forge in a state nobody can describe.
	if t.State != protocol.TaskQueued {
		return store.ErrConflict
	}

	t.State = protocol.TaskCancelled
	t.UpdatedAt = at

	finished := at
	t.FinishedAt = &finished

	return nil
}

// busyPartitionsLocked reports which partitions currently have a running task.
func (s *taskStore) busyPartitionsLocked() map[string]bool {
	busy := make(map[string]bool)

	for _, t := range s.tasks {
		if t.State == protocol.TaskRunning && t.PartitionKey != "" {
			busy[t.PartitionKey] = true
		}
	}

	return busy
}

func (s *taskStore) releaseExpiredLocked(now time.Time) int {
	released := 0

	for _, t := range s.tasks {
		if t.State != protocol.TaskRunning || !t.Lease.Expired(now) {
			continue
		}

		t.State = protocol.TaskQueued
		t.Lease = nil
		t.StartedAt = nil
		t.UpdatedAt = now

		released++
	}

	return released
}

// checkLease enforces fencing: a transition is only valid from the worker
// holding the current lease.
// checkFinishOrder refuses a completion timestamped before the task started.
//
// PostgreSQL enforces this with a check constraint. The two backends have to be
// indistinguishable to a caller, and a row whose finished_at precedes its
// started_at makes every duration report and every audit reconstruction wrong —
// so this is the contract rather than a quirk of one schema.
func checkFinishOrder(t *store.Task, at time.Time) error {
	if t.StartedAt == nil || !at.Before(*t.StartedAt) {
		return nil
	}

	return fmt.Errorf("%w: a task cannot finish (%s) before it started (%s)",
		store.ErrConflict, at.UTC(), t.StartedAt.UTC())
}

func checkLease(t *store.Task, token string) error {
	if t.State != protocol.TaskRunning || t.Lease == nil {
		return store.ErrLeaseLost
	}
	if t.Lease.Token != token {
		return store.ErrLeaseLost
	}
	return nil
}

func cloneTask(t *store.Task) *store.Task {
	clone := *t

	if t.Lease != nil {
		lease := *t.Lease
		clone.Lease = &lease
	}
	if t.Payload != nil {
		clone.Payload = append([]byte(nil), t.Payload...)
	}
	if t.Result != nil {
		clone.Result = append([]byte(nil), t.Result...)
	}
	if t.Error != nil {
		cause := *t.Error
		clone.Error = &cause
	}
	if t.StartedAt != nil {
		started := *t.StartedAt
		clone.StartedAt = &started
	}
	if t.FinishedAt != nil {
		finished := *t.FinishedAt
		clone.FinishedAt = &finished
	}

	return &clone
}

func containsState(states []protocol.TaskState, s protocol.TaskState) bool {
	for _, x := range states {
		if x == s {
			return true
		}
	}
	return false
}

func containsKind(kinds []protocol.TaskKind, k protocol.TaskKind) bool {
	for _, x := range kinds {
		if x == k {
			return true
		}
	}
	return false
}

var _ store.TaskStore = (*taskStore)(nil)
