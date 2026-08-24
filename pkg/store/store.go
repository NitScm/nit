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
	// ByTokenHash finds a session by the hash of its token, across every
	// tenant.
	//
	// No tenant argument, and that is the point rather than an omission: a
	// token is what *resolves* the tenant, so a caller cannot be asked to
	// supply the answer to the question it is asking. The hash is unique
	// across the deployment by constraint, so the lookup is unambiguous, and
	// the tenant then comes off the session — which is authoritative.
	ByTokenHash(ctx context.Context, hash []byte) (*Session, error)

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
// There is no update and no delete. That is not an omission: the database
// refuses both, and an application that could delete an audit record would be
// one bug away from erasing the evidence that it enforced anything.
//
// Retention is an operator action, and it has its own interface — see
// AuditPruner, which a Store may implement and which nothing on the request
// path may reach.
type AuditStore interface {
	Append(ctx context.Context, records ...*AuditRecord) error
	Query(ctx context.Context, q AuditQuery) ([]*AuditRecord, error)
}

// AuditPruner removes audit records older than a cutoff.
//
// Deliberately *not* part of Store, and deliberately not reachable through
// AuditStore. A backend that implements it exposes a capability the server and
// the workers can never call, because they hold a Store and would have to type
// assert their way out of the contract to find it. `nitctl audit prune` is the
// only caller, which is what makes a purge an operator action rather than
// something a code path can do by accident.
//
// An implementation carries three obligations:
//
//   - **It must delete only what is older than the cutoff.** The caller writes
//     a record of the purge before starting; that record is newer than any
//     cutoff and must survive it.
//   - **It must restore the append-only protection it lifted**, including when
//     the prune fails partway. On a backend where lifting it is DDL, and
//     therefore cannot be rolled back, an interrupted prune leaves the table
//     unprotected — which is what Result.GuardsWereMissing is for.
//   - **It must work in batches.** A retention sweep on a year of records must
//     not hold a lock for the length of one statement.
type AuditPruner interface {
	// CountAuditBefore reports how many records a prune with this cutoff would
	// remove.
	//
	// Part of the interface rather than a convenience, because a tool that
	// deletes evidence without being able to say how much is not one an
	// operator should run: there is no undo, and "about a year's worth" is not
	// a number anybody can approve.
	CountAuditBefore(ctx context.Context, before time.Time) (int64, error)

	// PruneAudit removes records whose occurred_at precedes before, in batches
	// of at most batch rows, until none are left.
	PruneAudit(ctx context.Context, before time.Time, batch int) (PruneResult, error)
}

// tenantKey is unexported so no other package can collide with it.
type tenantKey struct{}

// WithTenant returns a context naming whose data the operations under it may
// touch.
//
// A backend may enforce it — the PostgreSQL one does, with row-level security —
// so this is not documentation. A context without a tenant reaches a database
// that has RLS in force and sees *nothing*, which is the intended answer: a
// caller that forgot gets an empty result rather than somebody else's rows.
//
// Set it once, where the tenant is resolved, and let it flow. Threading it
// through arguments instead is what produced the failure this exists to
// prevent: a parameter is easy to pass wrong, and a wrong tenant is silent.
func WithTenant(ctx context.Context, tenant policy.TenantID) context.Context {
	return context.WithValue(ctx, tenantKey{}, tenant)
}

// TenantFrom returns the tenant a context names, or the empty string.
//
// Empty is meaningful: it is what a backend translates into "match nothing".
func TenantFrom(ctx context.Context) policy.TenantID {
	tenant, _ := ctx.Value(tenantKey{}).(policy.TenantID)
	return tenant
}

// TaskNotifier is implemented by backends that can say when a task changed,
// instead of being asked.
//
// Optional, like AuditPruner. A backend that does not implement it is not
// deficient: PostgreSQL has LISTEN/NOTIFY and MySQL has nothing equivalent, so
// a caller has to work without one either way.
//
// # A notification is a hint, never a substitute for reading
//
// This is the whole contract, and getting it wrong is how a client waits
// forever. A notification may be dropped — the listening connection can fail
// and reconnect across a change — duplicated, or arrive before the caller can
// observe the row. So a caller must keep a poll running as its liveness
// guarantee and treat notifications as what shortens the wait, not what ends
// it.
//
// Implemented properly, that turns a 500 ms poll into a slow backstop and a
// notification into the thing that actually wakes a developer's `nit push`.
type TaskNotifier interface {
	// WatchTasks delivers the id of each task whose state changed, until ctx
	// is done. The channel is closed when the watch ends.
	//
	// It must not block on a slow consumer: an implementation drops rather
	// than waits, which the "hint, never a substitute" rule above is what
	// makes safe.
	WatchTasks(ctx context.Context) (<-chan ID, error)
}

// PruneResult reports what a purge did, and what it found.
type PruneResult struct {
	Removed int64

	// GuardsWereMissing reports that the append-only protection was already
	// absent when the prune began.
	//
	// It means a previous purge did not finish, and that the table has been
	// writable — and erasable — since. An operator needs to be told, loudly:
	// between those two moments, the audit trail proves nothing.
	GuardsWereMissing bool
}
