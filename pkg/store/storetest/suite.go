// Package storetest is a conformance suite every store implementation must
// pass.
//
// The queue semantics are subtle — partition exclusion, lease expiry, fencing
// tokens, idempotent submission — and an implementation that gets any of them
// slightly wrong fails in production in ways that are extremely hard to
// reproduce: a stranded branch, a duplicated commit, two workers pushing the
// same task. Writing the tests once and running them against every backend is
// the only way to keep the in-memory and PostgreSQL stores honest about the
// same contract.
package storetest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// Factory builds a fresh, empty store for one test.
type Factory func(t *testing.T) store.Store

// Run executes the whole suite against an implementation.
func Run(t *testing.T, newStore Factory) {
	t.Helper()

	t.Run("TaskIdempotency", func(t *testing.T) { testTaskIdempotency(t, newStore) })
	t.Run("ClaimIsFIFO", func(t *testing.T) { testClaimIsFIFO(t, newStore) })
	t.Run("PartitionExclusion", func(t *testing.T) { testPartitionExclusion(t, newStore) })
	t.Run("UnpartitionedRunInParallel", func(t *testing.T) { testUnpartitionedRunInParallel(t, newStore) })
	t.Run("KindFiltering", func(t *testing.T) { testKindFiltering(t, newStore) })
	t.Run("LeaseExpiryReclaims", func(t *testing.T) { testLeaseExpiryReclaims(t, newStore) })
	t.Run("FencingRejectsStaleToken", func(t *testing.T) { testFencingRejectsStaleToken(t, newStore) })
	t.Run("Heartbeat", func(t *testing.T) { testHeartbeat(t, newStore) })
	t.Run("FailRequeue", func(t *testing.T) { testFailRequeue(t, newStore) })
	t.Run("QueuePosition", func(t *testing.T) { testQueuePosition(t, newStore) })
	t.Run("Cancel", func(t *testing.T) { testCancel(t, newStore) })
	t.Run("SyncPointCompareAndSet", func(t *testing.T) { testSyncPointCompareAndSet(t, newStore) })
	t.Run("ArtifactDeduplication", func(t *testing.T) { testArtifactDeduplication(t, newStore) })
	t.Run("ArtifactExpiry", func(t *testing.T) { testArtifactExpiry(t, newStore) })
	t.Run("AuditAppendAndQuery", func(t *testing.T) { testAuditAppendAndQuery(t, newStore) })
	t.Run("ConcurrentClaims", func(t *testing.T) { testConcurrentClaims(t, newStore) })
	t.Run("Sessions", func(t *testing.T) { testSessions(t, newStore) })
}

func testSessions(t *testing.T, newStore Factory) {
	f := setup(t, newStore)
	ctx := context.Background()

	hash := []byte("hashed-token-bytes")
	expiry := f.now.Add(24 * time.Hour)

	created, err := f.store.Sessions().Create(ctx, &store.Session{
		TenantID:  policy.DefaultTenant,
		UserID:    f.user.ID,
		TokenHash: hash,
		Label:     "laptop",
		CreatedAt: f.now,
		ExpiresAt: &expiry,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	found, err := f.store.Sessions().ByTokenHash(ctx, policy.DefaultTenant, hash)
	if err != nil {
		t.Fatalf("ByTokenHash: %v", err)
	}
	if found.ID != created.ID {
		t.Errorf("got session %s, want %s", found.ID, created.ID)
	}
	if !found.Active(f.now) {
		t.Error("a fresh session must be active")
	}
	if found.Active(expiry.Add(time.Second)) {
		t.Error("an expired session must not be active")
	}

	if _, err := f.store.Sessions().ByTokenHash(ctx, policy.DefaultTenant, []byte("other")); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound for an unknown token", err)
	}

	if err := f.store.Sessions().Touch(ctx, created.ID, f.now.Add(time.Minute)); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	// A revoked session must stay findable — a lookup has to be able to report
	// "revoked" rather than "unknown" — but must no longer be active.
	revokedAt := f.now.Add(2 * time.Hour)

	if err := f.store.Sessions().Revoke(ctx, created.ID, revokedAt); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if err := f.store.Sessions().Revoke(ctx, created.ID, revokedAt.Add(time.Hour)); err != nil {
		t.Fatalf("Revoke again: %v", err)
	}

	found, err = f.store.Sessions().ByID(ctx, created.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if found.Active(revokedAt.Add(time.Second)) {
		t.Error("a revoked session must not be active")
	}
	if found.RevokedAt == nil || !found.RevokedAt.Equal(revokedAt) {
		t.Errorf("RevokedAt = %v, want the first revocation instant %v", found.RevokedAt, revokedAt)
	}
	if found.LastUsedAt == nil {
		t.Error("Touch did not record usage")
	}

	sessions, err := f.store.Sessions().ListByUser(ctx, f.user.ID)
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(sessions) != 1 {
		t.Errorf("got %d sessions, want 1", len(sessions))
	}
}

// Workers claim concurrently in production. This is the test that would catch a
// dispatch query which looks correct sequentially but hands the same task to
// two workers, or serializes every worker behind one lock.
func testConcurrentClaims(t *testing.T, newStore Factory) {
	f := setup(t, newStore)

	const (
		branches         = 4
		tasksPerBranch   = 3
		concurrentClaims = 8
	)

	offset := time.Duration(0)
	for b := range branches {
		for i := range tasksPerBranch {
			offset += time.Second
			branch := "feature/" + string(rune('a'+b))
			f.create(t, f.newTask(protocol.TaskPush, branch, fmt.Sprintf("req-%d-%d", b, i), offset))
		}
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		claimed []*store.Task
	)

	for w := range concurrentClaims {
		wg.Add(1)

		go func(worker int) {
			defer wg.Done()

			task, err := f.claim(t, fmt.Sprintf("worker-%d", worker), f.now.Add(time.Hour))
			if errors.Is(err, store.ErrNoTask) {
				return
			}
			if err != nil {
				t.Errorf("Claim: %v", err)
				return
			}

			mu.Lock()
			claimed = append(claimed, task)
			mu.Unlock()
		}(w)
	}

	wg.Wait()

	// One task per branch may run at a time, so exactly `branches` claims must
	// have succeeded.
	if len(claimed) != branches {
		t.Errorf("%d claims succeeded, want %d (one per branch)", len(claimed), branches)
	}

	seenTask := make(map[store.ID]bool)
	seenPartition := make(map[string]bool)

	for _, task := range claimed {
		if seenTask[task.ID] {
			t.Errorf("task %s was claimed twice", task.ID)
		}
		seenTask[task.ID] = true

		if seenPartition[task.PartitionKey] {
			t.Errorf("two tasks claimed on partition %q", task.PartitionKey)
		}
		seenPartition[task.PartitionKey] = true
	}
}

// fixture holds the records a task needs to reference.
type fixture struct {
	store     store.Store
	user      *store.User
	workspace *store.Workspace
	repo      *store.Repository
	now       time.Time
}

func setup(t *testing.T, newStore Factory) *fixture {
	t.Helper()

	ctx := context.Background()
	s := newStore(t)

	// A fixed instant: every lease assertion is about ordering, never about
	// how long the test took to run.
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)

	user, err := s.Users().Upsert(ctx, &store.User{
		TenantID:     policy.DefaultTenant,
		PolicyUserID: "dev",
		Email:        "dev@example.com",
		CreatedAt:    now,
		UpdatedAt:    now,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}

	workspace, err := s.Workspaces().Create(ctx, &store.Workspace{
		TenantID:  policy.DefaultTenant,
		UserID:    user.ID,
		Label:     "laptop",
		CreatedAt: now,
	})
	if err != nil {
		t.Fatalf("create workspace: %v", err)
	}

	if err := s.Repositories().Reconcile(ctx, policy.DefaultTenant, []*store.Repository{{
		TenantID:      policy.DefaultTenant,
		PolicyRepoID:  "backend-api",
		Remote:        "https://example.com/backend-api.git",
		Forge:         "github",
		DefaultBranch: "main",
		PolicyVersion: "sha256:test",
		CreatedAt:     now,
		UpdatedAt:     now,
	}}); err != nil {
		t.Fatalf("reconcile repositories: %v", err)
	}

	repo, err := s.Repositories().ByPolicyID(ctx, policy.DefaultTenant, "backend-api")
	if err != nil {
		t.Fatalf("load repository: %v", err)
	}

	return &fixture{store: s, user: user, workspace: workspace, repo: repo, now: now}
}

// newTask builds a task; offset spreads creation times so FIFO order is
// unambiguous.
func (f *fixture) newTask(kind protocol.TaskKind, branch, requestID string, offset time.Duration) *store.Task {
	partition := ""
	if kind == protocol.TaskPush {
		partition = string(f.repo.PolicyRepoID) + ":" + branch
	}

	return &store.Task{
		TenantID:     policy.DefaultTenant,
		RequestID:    requestID,
		Kind:         kind,
		State:        protocol.TaskQueued,
		UserID:       f.user.ID,
		WorkspaceID:  f.workspace.ID,
		RepositoryID: f.repo.ID,
		Branch:       branch,
		PartitionKey: partition,
		Payload:      []byte(`{}`),
		CreatedAt:    f.now.Add(offset),
	}
}

func (f *fixture) create(t *testing.T, task *store.Task) *store.Task {
	t.Helper()

	created, err := f.store.Tasks().Create(context.Background(), task)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	return created
}

func (f *fixture) claim(t *testing.T, holder string, at time.Time, kinds ...protocol.TaskKind) (*store.Task, error) {
	t.Helper()

	return f.store.Tasks().Claim(context.Background(), store.ClaimOptions{
		Holder:   holder,
		Kinds:    kinds,
		LeaseFor: 30 * time.Second,
		Now:      at,
	})
}

// ---------------------------------------------------------------------------

// A retried submission must return the original task, never create a second
// one: otherwise a network failure mid-push becomes a duplicated commit.
func testTaskIdempotency(t *testing.T, newStore Factory) {
	f := setup(t, newStore)
	ctx := context.Background()

	first := f.create(t, f.newTask(protocol.TaskPush, "main", "req-1", 0))

	_, err := f.store.Tasks().Create(ctx, f.newTask(protocol.TaskPush, "main", "req-1", time.Second))
	if !errors.Is(err, store.ErrDuplicateRequest) {
		t.Fatalf("got %v, want ErrDuplicateRequest", err)
	}

	existing, err := f.store.Tasks().ByRequestID(ctx, policy.DefaultTenant, "req-1")
	if err != nil {
		t.Fatalf("ByRequestID: %v", err)
	}
	if existing.ID != first.ID {
		t.Errorf("got task %s, want the original %s", existing.ID, first.ID)
	}
}

func testClaimIsFIFO(t *testing.T, newStore Factory) {
	f := setup(t, newStore)

	// Different partitions, so ordering is decided by age alone.
	second := f.create(t, f.newTask(protocol.TaskPush, "b", "req-2", 2*time.Second))
	first := f.create(t, f.newTask(protocol.TaskPush, "a", "req-1", time.Second))

	got, err := f.claim(t, "worker-1", f.now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got.ID != first.ID {
		t.Errorf("claimed %s, want the older task %s", got.ID, first.ID)
	}

	got, err = f.claim(t, "worker-2", f.now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got.ID != second.ID {
		t.Errorf("claimed %s, want %s", got.ID, second.ID)
	}
}

// The property that replaces a distributed lock: one running task per branch.
func testPartitionExclusion(t *testing.T, newStore Factory) {
	f := setup(t, newStore)

	f.create(t, f.newTask(protocol.TaskPush, "main", "req-1", time.Second))
	f.create(t, f.newTask(protocol.TaskPush, "main", "req-2", 2*time.Second))

	if _, err := f.claim(t, "worker-1", f.now.Add(time.Minute)); err != nil {
		t.Fatalf("first Claim: %v", err)
	}

	_, err := f.claim(t, "worker-2", f.now.Add(time.Minute))
	if !errors.Is(err, store.ErrNoTask) {
		t.Fatalf("got %v, want ErrNoTask: a second task on a busy branch must not be dispatched", err)
	}
}

// Pull tasks are read-only and must not serialize against anything.
func testUnpartitionedRunInParallel(t *testing.T, newStore Factory) {
	f := setup(t, newStore)

	f.create(t, f.newTask(protocol.TaskPull, "main", "req-1", time.Second))
	f.create(t, f.newTask(protocol.TaskPull, "main", "req-2", 2*time.Second))

	if _, err := f.claim(t, "worker-1", f.now.Add(time.Minute)); err != nil {
		t.Fatalf("first Claim: %v", err)
	}
	if _, err := f.claim(t, "worker-2", f.now.Add(time.Minute)); err != nil {
		t.Fatalf("second Claim: %v — pull tasks must run in parallel", err)
	}
}

func testKindFiltering(t *testing.T, newStore Factory) {
	f := setup(t, newStore)

	f.create(t, f.newTask(protocol.TaskPush, "main", "req-1", time.Second))
	pull := f.create(t, f.newTask(protocol.TaskPull, "main", "req-2", 2*time.Second))

	got, err := f.claim(t, "puller", f.now.Add(time.Minute), protocol.TaskPull)
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if got.ID != pull.ID {
		t.Errorf("claimed %s, want the pull task %s", got.ID, pull.ID)
	}
}

// A worker that dies must not strand its branch.
func testLeaseExpiryReclaims(t *testing.T, newStore Factory) {
	f := setup(t, newStore)
	ctx := context.Background()

	task := f.create(t, f.newTask(protocol.TaskPush, "main", "req-1", time.Second))

	claimed, err := f.claim(t, "doomed-worker", f.now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// Still leased: nothing else may take it.
	if _, err := f.claim(t, "worker-2", f.now.Add(time.Minute+time.Second)); !errors.Is(err, store.ErrNoTask) {
		t.Fatalf("got %v, want ErrNoTask while the lease holds", err)
	}

	afterExpiry := claimed.Lease.ExpiresAt.Add(time.Second)

	released, err := f.store.Tasks().ReleaseExpired(ctx, afterExpiry)
	if err != nil {
		t.Fatalf("ReleaseExpired: %v", err)
	}
	if released != 1 {
		t.Errorf("released %d tasks, want 1", released)
	}

	retaken, err := f.claim(t, "worker-2", afterExpiry)
	if err != nil {
		t.Fatalf("Claim after expiry: %v", err)
	}
	if retaken.ID != task.ID {
		t.Errorf("claimed %s, want %s", retaken.ID, task.ID)
	}
	if retaken.Attempts != 2 {
		t.Errorf("Attempts = %d, want 2", retaken.Attempts)
	}
}

// The fencing property: a worker whose lease lapsed must not be able to finish
// a task another worker now owns.
func testFencingRejectsStaleToken(t *testing.T, newStore Factory) {
	f := setup(t, newStore)
	ctx := context.Background()

	task := f.create(t, f.newTask(protocol.TaskPush, "main", "req-1", time.Second))

	stale, err := f.claim(t, "worker-1", f.now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	afterExpiry := stale.Lease.ExpiresAt.Add(time.Second)

	if _, err := f.store.Tasks().ReleaseExpired(ctx, afterExpiry); err != nil {
		t.Fatalf("ReleaseExpired: %v", err)
	}
	if _, err := f.claim(t, "worker-2", afterExpiry); err != nil {
		t.Fatalf("second Claim: %v", err)
	}

	err = f.store.Tasks().Complete(ctx, task.ID, stale.Lease.Token, []byte(`{}`), afterExpiry)
	if !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("got %v, want ErrLeaseLost", err)
	}

	err = f.store.Tasks().Heartbeat(ctx, task.ID, stale.Lease.Token, afterExpiry.Add(time.Minute))
	if !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("heartbeat: got %v, want ErrLeaseLost", err)
	}

	current, err := f.store.Tasks().ByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if current.State != protocol.TaskRunning {
		t.Errorf("state = %s, want running: the stale worker must not have completed it", current.State)
	}
}

func testHeartbeat(t *testing.T, newStore Factory) {
	f := setup(t, newStore)
	ctx := context.Background()

	f.create(t, f.newTask(protocol.TaskPush, "main", "req-1", time.Second))

	claimed, err := f.claim(t, "worker-1", f.now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	extended := claimed.Lease.ExpiresAt.Add(time.Hour)

	if err := f.store.Tasks().Heartbeat(ctx, claimed.ID, claimed.Lease.Token, extended); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}

	// The old expiry has passed but the extension holds, so nothing is released.
	released, err := f.store.Tasks().ReleaseExpired(ctx, claimed.Lease.ExpiresAt.Add(time.Second))
	if err != nil {
		t.Fatalf("ReleaseExpired: %v", err)
	}
	if released != 0 {
		t.Errorf("released %d tasks, want 0: the heartbeat should have kept the lease", released)
	}
}

func testFailRequeue(t *testing.T, newStore Factory) {
	f := setup(t, newStore)
	ctx := context.Background()

	task := f.create(t, f.newTask(protocol.TaskPush, "main", "req-1", time.Second))

	claimed, err := f.claim(t, "worker-1", f.now.Add(time.Minute))
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}

	cause := &protocol.Error{Code: protocol.CodeConflict, Message: "patch no longer applies"}
	at := f.now.Add(2 * time.Minute)

	if err := f.store.Tasks().Fail(ctx, claimed.ID, claimed.Lease.Token, cause, true, at); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	requeued, err := f.store.Tasks().ByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if requeued.State != protocol.TaskQueued {
		t.Errorf("state = %s, want queued", requeued.State)
	}
	if requeued.Attempts != 1 {
		t.Errorf("Attempts = %d, want the counter preserved across a requeue", requeued.Attempts)
	}

	// Terminal failure this time.
	claimed, err = f.claim(t, "worker-2", at.Add(time.Second))
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if err := f.store.Tasks().Fail(ctx, claimed.ID, claimed.Lease.Token, cause, false, at.Add(time.Minute)); err != nil {
		t.Fatalf("Fail: %v", err)
	}

	failed, err := f.store.Tasks().ByID(ctx, task.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if failed.State != protocol.TaskFailed {
		t.Errorf("state = %s, want failed", failed.State)
	}
	if failed.Error == nil || failed.Error.Code != protocol.CodeConflict {
		t.Errorf("error = %v, want the failure cause preserved", failed.Error)
	}
	if failed.FinishedAt == nil {
		t.Error("FinishedAt not set on a terminal failure")
	}
}

func testQueuePosition(t *testing.T, newStore Factory) {
	f := setup(t, newStore)
	ctx := context.Background()

	first := f.create(t, f.newTask(protocol.TaskPush, "main", "req-1", time.Second))
	second := f.create(t, f.newTask(protocol.TaskPush, "main", "req-2", 2*time.Second))
	third := f.create(t, f.newTask(protocol.TaskPush, "main", "req-3", 3*time.Second))

	for want, id := range map[int]store.ID{0: first.ID, 1: second.ID, 2: third.ID} {
		got, err := f.store.Tasks().QueuePosition(ctx, id)
		if err != nil {
			t.Fatalf("QueuePosition: %v", err)
		}
		if got != want {
			t.Errorf("position of %s = %d, want %d", id, got, want)
		}
	}

	// Once the first is running it still counts as ahead of the others.
	if _, err := f.claim(t, "worker-1", f.now.Add(time.Minute)); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	got, err := f.store.Tasks().QueuePosition(ctx, second.ID)
	if err != nil {
		t.Fatalf("QueuePosition: %v", err)
	}
	if got != 1 {
		t.Errorf("position = %d, want 1 with one task running ahead", got)
	}
}

func testCancel(t *testing.T, newStore Factory) {
	f := setup(t, newStore)
	ctx := context.Background()

	queued := f.create(t, f.newTask(protocol.TaskPush, "a", "req-1", time.Second))
	running := f.create(t, f.newTask(protocol.TaskPush, "b", "req-2", 2*time.Second))

	if err := f.store.Tasks().Cancel(ctx, queued.ID, f.now.Add(time.Minute)); err != nil {
		t.Fatalf("Cancel: %v", err)
	}

	got, err := f.store.Tasks().ByID(ctx, queued.ID)
	if err != nil {
		t.Fatalf("ByID: %v", err)
	}
	if got.State != protocol.TaskCancelled {
		t.Errorf("state = %s, want cancelled", got.State)
	}

	if _, err := f.claim(t, "worker-1", f.now.Add(time.Minute)); err != nil {
		t.Fatalf("Claim: %v", err)
	}

	// A running task belongs to its worker.
	if err := f.store.Tasks().Cancel(ctx, running.ID, f.now.Add(2*time.Minute)); !errors.Is(err, store.ErrConflict) {
		t.Errorf("got %v, want ErrConflict when cancelling a running task", err)
	}
}

// Two operations on the same workspace and branch must not be able to leave the
// client believing it is projected from a commit it never received.
func testSyncPointCompareAndSet(t *testing.T, newStore Factory) {
	f := setup(t, newStore)
	ctx := context.Background()

	sp := &store.SyncPoint{
		TenantID:       policy.DefaultTenant,
		WorkspaceID:    f.workspace.ID,
		RepositoryID:   f.repo.ID,
		Branch:         "main",
		UpstreamCommit: "aaaa1111",
		PolicyVersion:  "sha256:test",
		CreatedAt:      f.now,
		UpdatedAt:      f.now,
	}

	if err := f.store.SyncPoints().Put(ctx, sp); err != nil {
		t.Fatalf("Put: %v", err)
	}

	stale := *sp
	stale.UpstreamCommit = "cccc3333"

	if err := f.store.SyncPoints().CompareAndSet(ctx, &stale, "bbbb2222"); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("got %v, want ErrConflict on a stale expectation", err)
	}

	advanced := *sp
	advanced.UpstreamCommit = "bbbb2222"
	advanced.UpdatedAt = f.now.Add(time.Minute)

	if err := f.store.SyncPoints().CompareAndSet(ctx, &advanced, "aaaa1111"); err != nil {
		t.Fatalf("CompareAndSet: %v", err)
	}

	got, err := f.store.SyncPoints().Get(ctx, f.workspace.ID, f.repo.ID, "main")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.UpstreamCommit != "bbbb2222" {
		t.Errorf("UpstreamCommit = %q, want %q", got.UpstreamCommit, "bbbb2222")
	}
}

func testArtifactDeduplication(t *testing.T, newStore Factory) {
	f := setup(t, newStore)
	ctx := context.Background()

	a := &store.Artifact{
		TenantID:         policy.DefaultTenant,
		Digest:           "sha256:deadbeef",
		Kind:             store.ArtifactPushPatch,
		Size:             128,
		UncompressedSize: 512,
		Encoding:         protocol.EncodingZstd,
		Locator:          "de/ad/deadbeef",
		CreatedAt:        f.now,
	}

	first, err := f.store.Artifacts().Create(ctx, a)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	second, err := f.store.Artifacts().Create(ctx, a)
	if err != nil {
		t.Fatalf("Create again: %v", err)
	}

	if first.ID != second.ID {
		t.Errorf("identical bytes produced two artifacts: %s and %s", first.ID, second.ID)
	}
}

func testArtifactExpiry(t *testing.T, newStore Factory) {
	f := setup(t, newStore)
	ctx := context.Background()

	expiry := f.now.Add(time.Hour)

	if _, err := f.store.Artifacts().Create(ctx, &store.Artifact{
		TenantID:  policy.DefaultTenant,
		Digest:    "sha256:expiring",
		Kind:      store.ArtifactPullPatch,
		Size:      1,
		Encoding:  protocol.EncodingZstd,
		Locator:   "ex/pi/expiring",
		CreatedAt: f.now,
		ExpiresAt: &expiry,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if _, err := f.store.Artifacts().Create(ctx, &store.Artifact{
		TenantID:  policy.DefaultTenant,
		Digest:    "sha256:permanent",
		Kind:      store.ArtifactPushPatch,
		Size:      1,
		Encoding:  protocol.EncodingZstd,
		Locator:   "pe/rm/permanent",
		CreatedAt: f.now,
	}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	expired, err := f.store.Artifacts().Expired(ctx, expiry.Add(time.Second), 10)
	if err != nil {
		t.Fatalf("Expired: %v", err)
	}
	if len(expired) != 1 || expired[0].Digest != "sha256:expiring" {
		t.Fatalf("expired = %v, want only the expiring artifact", expired)
	}

	if err := f.store.Artifacts().Delete(ctx, expired[0].ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := f.store.Artifacts().ByDigest(ctx, policy.DefaultTenant, "sha256:expiring"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound after deletion", err)
	}
}

func testAuditAppendAndQuery(t *testing.T, newStore Factory) {
	f := setup(t, newStore)
	ctx := context.Background()

	records := []*store.AuditRecord{
		{
			TenantID:      policy.DefaultTenant,
			OccurredAt:    f.now,
			ActorUserID:   f.user.ID,
			ActorLabel:    "dev",
			Action:        "push.denied",
			RepositoryID:  f.repo.ID,
			Branch:        "main",
			Path:          "secrets/prod.env",
			Effect:        policy.EffectDeny,
			Reason:        policy.ReasonDeniedByRule,
			RuleID:        "secrets-are-platform-only",
			PolicyVersion: "sha256:test",
			RequestID:     "req-1",
		},
		{
			TenantID:      policy.DefaultTenant,
			OccurredAt:    f.now.Add(time.Second),
			ActorUserID:   f.user.ID,
			ActorLabel:    "dev",
			Action:        "push.rejected",
			RepositoryID:  f.repo.ID,
			Branch:        "main",
			PolicyVersion: "sha256:test",
			RequestID:     "req-1",
		},
	}

	if err := f.store.Audit().Append(ctx, records...); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err := f.store.Audit().Query(ctx, store.AuditQuery{
		Tenant:    policy.DefaultTenant,
		RequestID: "req-1",
		Limit:     10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d records, want 2", len(got))
	}

	// Newest first.
	if got[0].Action != "push.rejected" {
		t.Errorf("first record = %q, want the newest one", got[0].Action)
	}
	if got[1].RuleID != "secrets-are-platform-only" {
		t.Errorf("rule attribution lost: %q", got[1].RuleID)
	}
}
