package store

import (
	"context"
	"errors"
	"time"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
)

// Sentinel errors. Callers branch on these; no implementation may invent its
// own vocabulary for the same condition.
var (
	// ErrNotFound: the record does not exist.
	ErrNotFound = errors.New("store: not found")

	// ErrDuplicateRequest: a task already exists with this request id. The
	// caller must fetch and return that task instead of creating another —
	// this is what makes a retried push idempotent.
	ErrDuplicateRequest = errors.New("store: duplicate request id")

	// ErrNoTask: nothing was claimable. Not an error condition; an idle worker
	// sees this constantly.
	ErrNoTask = errors.New("store: no claimable task")

	// ErrLeaseLost: the presented fencing token is not the current one. The
	// worker must abandon its work immediately: another worker owns the task.
	ErrLeaseLost = errors.New("store: lease lost")

	// ErrConflict: the record changed under the caller.
	ErrConflict = errors.New("store: conflict")
)

// Store aggregates the repositories the control plane needs.
type Store interface {
	Users() UserStore
	Sessions() SessionStore
	Workspaces() WorkspaceStore
	Repositories() RepositoryStore
	SyncPoints() SyncPointStore
	Tasks() TaskStore
	Artifacts() ArtifactStore
	Audit() AuditStore

	// Close releases the underlying resources.
	Close() error
}

// UserStore persists people.
type UserStore interface {
	ByID(ctx context.Context, id ID) (*User, error)
	ByPolicyID(ctx context.Context, tenant policy.TenantID, policyID policy.UserID) (*User, error)

	// Upsert reconciles a user from the policy bundle.
	Upsert(ctx context.Context, u *User) (*User, error)
}

// SessionStore persists authentication tokens.
type SessionStore interface {
	Create(ctx context.Context, s *Session) (*Session, error)

	// ByTokenHash looks a session up by the hash of its token. It returns
	// revoked and expired sessions too: the caller decides, so that an expired
	// token can be reported as expired rather than as unknown.
	ByTokenHash(ctx context.Context, tenant policy.TenantID, hash []byte) (*Session, error)

	ByID(ctx context.Context, id ID) (*Session, error)

	ListByUser(ctx context.Context, userID ID) ([]*Session, error)

	// Touch records that the token was used, so an operator can see which
	// installations are still alive.
	Touch(ctx context.Context, id ID, at time.Time) error

	Revoke(ctx context.Context, id ID, at time.Time) error
}

// WorkspaceStore persists checkouts.
type WorkspaceStore interface {
	ByID(ctx context.Context, id ID) (*Workspace, error)
	ListByUser(ctx context.Context, userID ID) ([]*Workspace, error)

	Create(ctx context.Context, w *Workspace) (*Workspace, error)

	// Touch records activity, so an operator can spot workspaces that have gone
	// silent and whose sync points are stale.
	Touch(ctx context.Context, id ID, at time.Time) error
}

// RepositoryStore persists the repositories mirrored from the policy bundle.
type RepositoryStore interface {
	ByID(ctx context.Context, id ID) (*Repository, error)
	ByPolicyID(ctx context.Context, tenant policy.TenantID, policyID policy.RepoID) (*Repository, error)
	List(ctx context.Context, tenant policy.TenantID) ([]*Repository, error)

	// Reconcile makes stored repositories match the bundle. It never deletes:
	// a repository removed from the bundle still has tasks and audit records
	// pointing at it, and losing those to a configuration edit would be worse
	// than keeping a stale row.
	Reconcile(ctx context.Context, tenant policy.TenantID, repos []*Repository) error
}

// SyncPointStore persists workspace projections.
type SyncPointStore interface {
	Get(ctx context.Context, workspaceID, repositoryID ID, branch string) (*SyncPoint, error)

	// Put creates or replaces a sync point.
	Put(ctx context.Context, sp *SyncPoint) error

	// CompareAndSet replaces a sync point only if it still holds the expected
	// upstream commit.
	//
	// Two operations on the same workspace and branch must not be able to
	// interleave and leave the client believing it is projected from a commit
	// it never received. Returns ErrConflict when the expectation fails.
	CompareAndSet(ctx context.Context, sp *SyncPoint, expectedCommit string) error

	ListByWorkspace(ctx context.Context, workspaceID ID) ([]*SyncPoint, error)

	Delete(ctx context.Context, workspaceID, repositoryID ID, branch string) error
}

// ClaimOptions configures a dequeue attempt.
type ClaimOptions struct {
	// Holder identifies the worker, recorded on the lease for diagnostics.
	Holder string

	// Kinds restricts what the worker will take. Empty means any kind; this is
	// how "nit-worker --queues=pull" dedicates machines to read traffic.
	Kinds []protocol.TaskKind

	// LeaseFor is how long the claim is valid without a heartbeat. Too short
	// and a slow clone loses its task to a competitor; too long and a crashed
	// worker strands a branch. Workers must heartbeat well inside it.
	LeaseFor time.Duration

	// Now is the reference instant. Passing it explicitly, rather than reading
	// the clock inside the store, is what makes lease expiry testable without
	// sleeping.
	Now time.Time
}

// TaskFilter selects tasks for listing.
type TaskFilter struct {
	Tenant       policy.TenantID
	States       []protocol.TaskState
	Kinds        []protocol.TaskKind
	UserID       ID
	RepositoryID ID
	Branch       string

	Limit int
}

// TaskStore persists and dispatches queued work.
//
// The dispatch contract, which every implementation must honour:
//
//   - At most one task per non-empty PartitionKey is in the running state at
//     any instant. This is what serializes a branch, and it replaces a
//     distributed lock held across the whole clone-apply-push cycle.
//   - Tasks with an empty PartitionKey are unconstrained.
//   - Among claimable tasks, the oldest is taken first, so a branch's queue is
//     FIFO and a developer's push cannot be starved.
//   - Every state transition presents the fencing token from the claim.
type TaskStore interface {
	Create(ctx context.Context, t *Task) (*Task, error)

	ByID(ctx context.Context, id ID) (*Task, error)
	ByRequestID(ctx context.Context, tenant policy.TenantID, requestID string) (*Task, error)

	List(ctx context.Context, f TaskFilter) ([]*Task, error)

	// Claim atomically takes the next dispatchable task and leases it.
	// Returns ErrNoTask when nothing is available.
	Claim(ctx context.Context, opts ClaimOptions) (*Task, error)

	// Heartbeat extends a lease. Returns ErrLeaseLost if the token is stale.
	Heartbeat(ctx context.Context, id ID, token string, until time.Time) error

	// Complete marks a task succeeded and stores its result.
	Complete(ctx context.Context, id ID, token string, result []byte, at time.Time) error

	// Fail marks a task failed. When requeue is true the task returns to the
	// queue for another attempt instead of terminating.
	Fail(ctx context.Context, id ID, token string, cause *protocol.Error, requeue bool, at time.Time) error

	// ReleaseExpired returns tasks whose lease lapsed to the queued state and
	// reports how many were released.
	//
	// This is the recovery path for a worker that died mid-task, and it is the
	// reason a branch cannot be stranded by a crash.
	ReleaseExpired(ctx context.Context, now time.Time) (int, error)

	// QueuePosition reports how many tasks sit ahead of this one in its
	// partition. Zero means next. It is what the CLI shows while waiting, so a
	// queued push does not look hung.
	QueuePosition(ctx context.Context, id ID) (int, error)

	// Cancel terminates a queued task. A running task cannot be cancelled this
	// way: its worker holds the lease, and killing the record underneath it
	// would leave the forge in an unknown state.
	Cancel(ctx context.Context, id ID, at time.Time) error
}

// ArtifactStore persists blob metadata. The bytes live in the blob package.
type ArtifactStore interface {
	ByDigest(ctx context.Context, tenant policy.TenantID, digest string) (*Artifact, error)

	// Create records a blob. A digest that already exists is returned as-is
	// rather than duplicated: content addressing means identical bytes are the
	// same artifact.
	Create(ctx context.Context, a *Artifact) (*Artifact, error)

	// Expired lists artifacts past their expiry, for the garbage collector.
	Expired(ctx context.Context, now time.Time, limit int) ([]*Artifact, error)

	Delete(ctx context.Context, id ID) error
}

// AuditQuery selects audit records.
type AuditQuery struct {
	Tenant       policy.TenantID
	ActorUserID  ID
	RepositoryID ID
	RequestID    string
	Since        time.Time
	Until        time.Time

	Limit int
}

// AuditStore appends to the audit log.
//
// There is no update and no delete: retention is an operational concern handled
// with partitions and a privileged role, never by the application.
type AuditStore interface {
	Append(ctx context.Context, records ...*AuditRecord) error
	Query(ctx context.Context, q AuditQuery) ([]*AuditRecord, error)
}
