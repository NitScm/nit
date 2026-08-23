package postgres_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/store/postgres"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// A state change has to reach a listener, or the long poll silently falls back
// to its ticker and the feature is a no-op nobody notices.
func TestWatchTasksDeliversAStateChange(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("NIT_TEST_POSTGRES not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s := freshStore(dsn)(t).(*postgres.Store)

	changes, err := s.WatchTasks(ctx)
	if err != nil {
		t.Fatalf("WatchTasks: %v", err)
	}

	task := seedTask(t, ctx, s)

	// LISTEN is registered on its own connection; give it a moment to be in
	// place before the change, so this tests delivery rather than a race.
	time.Sleep(200 * time.Millisecond)

	// Claim moves the task from queued to running, which is what the trigger
	// fires on.
	if _, err := s.Tasks().Claim(ctx, store.ClaimOptions{
		Holder:   "watcher",
		LeaseFor: time.Minute,
		Now:      time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	select {
	case id := <-changes:
		if id != task.ID {
			t.Errorf("notified about %s, want %s", id, task.ID)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no notification arrived for a task that changed state")
	}
}

// A heartbeat rewrites lease_expires_at every few seconds. Waking every waiter
// for that would cost more than the poll it replaces, which is why the trigger
// is restricted to a change of state.
func TestAHeartbeatDoesNotNotify(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("NIT_TEST_POSTGRES not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	s := freshStore(dsn)(t).(*postgres.Store)

	seedTask(t, ctx, s)

	claimed, err := s.Tasks().Claim(ctx, store.ClaimOptions{
		Holder: "watcher", LeaseFor: time.Minute, Now: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Watched only now, so the claim's own notification is not in the way.
	changes, err := s.WatchTasks(ctx)
	if err != nil {
		t.Fatalf("WatchTasks: %v", err)
	}

	time.Sleep(200 * time.Millisecond)

	if err := s.Tasks().Heartbeat(ctx, claimed.ID, claimed.Lease.Token,
		time.Now().UTC().Add(2*time.Minute)); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	select {
	case id := <-changes:
		t.Errorf("a heartbeat notified about %s; only a change of state should", id)
	case <-time.After(time.Second):
	}
}

// seedTask creates the rows a task needs and returns it.
func seedTask(t *testing.T, ctx context.Context, s *postgres.Store) *store.Task {
	t.Helper()

	user, err := s.Users().Upsert(ctx, &store.User{
		TenantID: policy.DefaultTenant, PolicyUserID: "dev", Email: "dev@example.com",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	if err := s.Repositories().Reconcile(ctx, policy.DefaultTenant, []*store.Repository{{
		PolicyRepoID: "repo", Remote: "https://example.com/r.git",
		Forge: "generic", DefaultBranch: "main",
	}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	repo, err := s.Repositories().ByPolicyID(ctx, policy.DefaultTenant, "repo")
	if err != nil {
		t.Fatalf("repository: %v", err)
	}

	task, err := s.Tasks().Create(ctx, &store.Task{
		TenantID:     policy.DefaultTenant,
		RequestID:    "req-notify",
		Kind:         protocol.TaskPush,
		UserID:       user.ID,
		RepositoryID: repo.ID,
		Branch:       "main",
		PartitionKey: "repo:main",
		Payload:      []byte(`{}`),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	return task
}
