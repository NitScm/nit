package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

type taskStore struct{ pool *pgxpool.Pool }

const taskColumns = `
	id::text, tenant_id, request_id, kind, state,
	user_id::text, workspace_id::text, repository_id::text, branch, partition_key,
	payload, result, error, attempts,
	lease_holder, lease_token, lease_expires_at,
	created_at, updated_at, started_at, finished_at`

func scanTask(row pgx.Row) (*store.Task, error) {
	var (
		t            store.Task
		workspaceID  *string
		partitionKey *string
		result       []byte
		errorJSON    []byte
		leaseHolder  *string
		leaseToken   *string
		leaseExpires *time.Time
	)

	err := row.Scan(
		&t.ID, &t.TenantID, &t.RequestID, &t.Kind, &t.State,
		&t.UserID, &workspaceID, &t.RepositoryID, &t.Branch, &partitionKey,
		&t.Payload, &result, &errorJSON, &t.Attempts,
		&leaseHolder, &leaseToken, &leaseExpires,
		&t.CreatedAt, &t.UpdatedAt, &t.StartedAt, &t.FinishedAt,
	)
	if err != nil {
		return nil, mapError(err)
	}

	t.WorkspaceID = derefID(workspaceID)
	t.PartitionKey = derefString(partitionKey)
	t.Result = result

	if len(errorJSON) > 0 {
		var cause protocol.Error
		if err := json.Unmarshal(errorJSON, &cause); err != nil {
			return nil, err
		}
		t.Error = &cause
	}

	// The lease columns are constrained to move together (see
	// tasks_lease_consistency), so testing one is enough.
	if leaseToken != nil {
		t.Lease = &store.Lease{
			Holder:    derefString(leaseHolder),
			Token:     *leaseToken,
			ExpiresAt: derefTime(leaseExpires),
		}
	}

	return &t, nil
}

func (s *taskStore) Create(ctx context.Context, t *store.Task) (*store.Task, error) {
	state := t.State
	if state == "" {
		state = protocol.TaskQueued
	}

	createdAt := t.CreatedAt
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}

	payload := t.Payload
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}

	created, err := scanTask(s.pool.QueryRow(ctx, `
		INSERT INTO tasks (
			tenant_id, request_id, kind, state,
			user_id, workspace_id, repository_id, branch, partition_key,
			payload, created_at, updated_at
		) VALUES (
			$1, $2, $3, $4,
			$5::uuid, $6::uuid, $7::uuid, $8, $9,
			$10, $11, $11
		)
		RETURNING `+taskColumns,
		string(t.TenantID), t.RequestID, string(t.Kind), string(state),
		string(t.UserID), nullableID(t.WorkspaceID), string(t.RepositoryID), t.Branch, nullable(t.PartitionKey),
		payload, createdAt))

	// A repeated request id is not an error the caller has to reason about
	// twice: hand back the task that already exists, together with the
	// sentinel, exactly as the in-memory store does.
	if err != nil && isUniqueViolation(err, "tasks_request_id_unique") {
		existing, lookupErr := s.ByRequestID(ctx, t.TenantID, t.RequestID)
		if lookupErr != nil {
			return nil, store.ErrDuplicateRequest
		}
		return existing, store.ErrDuplicateRequest
	}

	return created, err
}

func (s *taskStore) ByID(ctx context.Context, id store.ID) (*store.Task, error) {
	return scanTask(s.pool.QueryRow(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE id = $1::uuid`, string(id)))
}

func (s *taskStore) ByRequestID(ctx context.Context, tenant policy.TenantID, requestID string) (*store.Task, error) {
	return scanTask(s.pool.QueryRow(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE tenant_id = $1 AND request_id = $2`,
		string(tenant), requestID))
}

func (s *taskStore) List(ctx context.Context, f store.TaskFilter) ([]*store.Task, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+taskColumns+`
		FROM tasks
		WHERE ($1 = '' OR tenant_id = $1)
		  AND ($2::text IS NULL OR user_id = $2::uuid)
		  AND ($3::text IS NULL OR repository_id = $3::uuid)
		  AND ($4 = '' OR branch = $4)
		  AND ($5::task_state[] IS NULL OR state = ANY($5::task_state[]))
		  AND ($6::task_kind[] IS NULL OR kind = ANY($6::task_kind[]))
		ORDER BY created_at DESC, id DESC
		LIMIT $7`,
		string(f.Tenant),
		nullableID(f.UserID),
		nullableID(f.RepositoryID),
		f.Branch,
		stateArray(f.States),
		kindArray(f.Kinds),
		limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []*store.Task
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}

	return out, mapError(rows.Err())
}

// Claim atomically dequeues and leases the next dispatchable task.
//
// Correctness here is subtle enough to be worth spelling out.
//
// **Exclusion is a unique constraint, not a lock.** A row in partition_leases
// is the right to run a task on one branch, and its primary key is what makes
// two workers unable to hold the same branch. The previous design serialized
// every claim in the deployment on one advisory lock (D15) — correct, and the
// throughput ceiling of the whole dispatch layer, because a busy repository
// slowed the claim path for every other repository.
//
// **Two authorities, both single-row and both atomic.** The insert into
// partition_leases decides who owns the branch. The `AND state = 'queued'` on
// the update decides who owns the *task*, which is what keeps two workers off
// one pull — a pull has no partition and therefore no lease. Neither depends on
// a scan being right, which is what made FOR UPDATE SKIP LOCKED insufficient
// here: two workers scanning concurrently lock different rows of the same
// branch, both see no running task, and both claim it.
//
// **A losing worker retries rather than reporting an empty queue.** It lost a
// race, not a search, and the retry now sees the winner's lease and skips that
// partition. Bounded, because a worker that has lost several times in a row is
// better off polling than spinning.
//
// Expired leases are reclaimed first, in the same transaction, so a worker that
// died mid-task cannot strand its branch until some separate reaper happens to
// run.
func (s *taskStore) Claim(ctx context.Context, opts store.ClaimOptions) (*store.Task, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	leaseFor := opts.LeaseFor
	if leaseFor <= 0 {
		leaseFor = time.Minute
	}

	for range claimAttempts {
		task, err := s.claimOnce(ctx, opts, now, leaseFor)
		if errors.Is(err, errLostTheRace) {
			continue
		}

		return task, err
	}

	// Contended past the retry budget. Reporting no task is honest — this
	// worker has none *right now* — and it polls again in queue.poll.
	return nil, store.ErrNoTask
}

// claimAttempts bounds the retries a contended claim makes.
//
// Small on purpose. Each retry is a fresh transaction, so a worker that keeps
// losing is burning a connection on a queue that other workers are draining
// faster than it can. Polling again in a second is the cheaper answer.
const claimAttempts = 5

// errLostTheRace is internal: another worker took the branch or the task
// between this transaction's read and its write.
var errLostTheRace = errors.New("postgres: lost the claim race")

func (s *taskStore) claimOnce(ctx context.Context, opts store.ClaimOptions, now time.Time, leaseFor time.Duration) (*store.Task, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	defer tx.Rollback(ctx)

	if _, err := releaseExpired(ctx, tx, now); err != nil {
		return nil, err
	}

	var (
		id           string
		tenant       string
		partitionKey *string
	)

	// A left join rather than the old correlated subquery over tasks. The
	// previous form asked "is anything running on this partition?" for every
	// candidate, so a queue whose head is full of busy branches was walked past
	// on every single claim.
	err = tx.QueryRow(ctx, `
		SELECT t.id::text, t.tenant_id, t.partition_key
		FROM tasks t
		LEFT JOIN partition_leases l
		  ON l.tenant_id = t.tenant_id AND l.partition_key = t.partition_key
		WHERE t.state = 'queued'
		  AND ($1::task_kind[] IS NULL OR t.kind = ANY($1::task_kind[]))
		  AND l.partition_key IS NULL
		ORDER BY t.created_at, t.id
		LIMIT 1`, kindArray(opts.Kinds)).Scan(&id, &tenant, &partitionKey)

	if errors.Is(err, pgx.ErrNoRows) {
		return nil, store.ErrNoTask
	}
	if err != nil {
		return nil, mapError(err)
	}

	if partitionKey != nil {
		// The authority on who owns the branch. ON CONFLICT DO NOTHING makes
		// losing cheap and unambiguous: no rows, no error, try another task.
		tag, err := tx.Exec(ctx, `
			INSERT INTO partition_leases (tenant_id, partition_key, task_id, acquired_at)
			VALUES ($1, $2, $3::uuid, $4)
			ON CONFLICT DO NOTHING`, tenant, *partitionKey, id, now)
		if err != nil {
			return nil, mapError(err)
		}

		if tag.RowsAffected() == 0 {
			return nil, errLostTheRace
		}
	}

	// The authority on who owns the task. It matters for pulls, which have no
	// partition and therefore no lease of their own.
	task, err := scanTask(tx.QueryRow(ctx, `
		UPDATE tasks
		SET state = 'running',
		    attempts = attempts + 1,
		    lease_holder = $2,
		    lease_token = gen_random_uuid()::text,
		    lease_expires_at = $3,
		    started_at = COALESCE(started_at, $4),
		    updated_at = $4
		WHERE id = $1::uuid AND state = 'queued'
		RETURNING `+taskColumns,
		id, opts.Holder, now.Add(leaseFor), now))

	if errors.Is(err, store.ErrNotFound) {
		return nil, errLostTheRace
	}
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, mapError(err)
	}

	return task, nil
}

func (s *taskStore) Heartbeat(ctx context.Context, id store.ID, token string, until time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks
		SET lease_expires_at = $3, updated_at = $3
		WHERE id = $1::uuid AND state = 'running' AND lease_token = $2`,
		string(id), token, until)
	if err != nil {
		return mapError(err)
	}

	return leaseResult(ctx, s.pool, id, tag.RowsAffected())
}

func (s *taskStore) Complete(ctx context.Context, id store.ID, token string, result []byte, at time.Time) error {
	if len(result) == 0 {
		result = []byte(`{}`)
	}

	return s.finish(ctx, id, token, `
		UPDATE tasks
		SET state = 'succeeded',
		    result = $3,
		    error = NULL,
		    lease_holder = NULL, lease_token = NULL, lease_expires_at = NULL,
		    updated_at = $4, finished_at = $4
		WHERE id = $1::uuid AND state = 'running' AND lease_token = $2`,
		result, at)
}

func (s *taskStore) Fail(ctx context.Context, id store.ID, token string, cause *protocol.Error, requeue bool, at time.Time) error {
	var causeJSON []byte
	if cause != nil {
		encoded, err := json.Marshal(cause)
		if err != nil {
			return err
		}
		causeJSON = encoded
	}

	// The attempt counter is deliberately not reset on requeue: it is what
	// lets a caller give up on a task that fails repeatedly instead of cycling
	// it through the queue forever.
	query := `
		UPDATE tasks
		SET state = 'failed',
		    error = $3,
		    lease_holder = NULL, lease_token = NULL, lease_expires_at = NULL,
		    updated_at = $4, finished_at = $4
		WHERE id = $1::uuid AND state = 'running' AND lease_token = $2`

	if requeue {
		query = `
			UPDATE tasks
			SET state = 'queued',
			    error = $3,
			    lease_holder = NULL, lease_token = NULL, lease_expires_at = NULL,
			    started_at = NULL,
			    updated_at = $4
			WHERE id = $1::uuid AND state = 'running' AND lease_token = $2`
	}

	return s.finish(ctx, id, token, query, causeJSON, at)
}

// finish applies a transition out of running and releases the branch with it.
//
// One transaction, and that is the point: a task that stopped running while its
// partition lease survived would block its branch for good, and a lease removed
// while the task kept running would let a second worker onto it. Neither is
// recoverable by anything watching from outside, so they commit together or
// not at all.
func (s *taskStore) finish(ctx context.Context, id store.ID, token, query string, payload []byte, at time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mapError(err)
	}
	defer tx.Rollback(ctx)

	// The lease goes first, matching the order a claim uses: partition_leases,
	// then tasks. Two transactions taking the same two rows in opposite orders
	// is a deadlock waiting for load, and MySQL found it under twelve workers
	// before this was made symmetric.
	//
	// Deleting before knowing whether the transition applies is safe because
	// this is a transaction: a stale token leaves the UPDATE matching nothing
	// and the rollback puts the lease back. Someone else's branch is never
	// taken from them.
	if _, err := tx.Exec(ctx,
		`DELETE FROM partition_leases WHERE task_id = $1::uuid`, string(id)); err != nil {
		return mapError(err)
	}

	tag, err := tx.Exec(ctx, query, string(id), token, payload, at)
	if err != nil {
		return mapError(err)
	}

	if tag.RowsAffected() != 1 {
		// Nothing moved: the task is gone, or somebody else holds it now. The
		// rollback restores the lease, which is theirs.
		return leaseResult(ctx, s.pool, id, tag.RowsAffected())
	}

	return mapError(tx.Commit(ctx))
}

func (s *taskStore) ReleaseExpired(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	defer tx.Rollback(ctx)

	released, err := releaseExpired(ctx, tx, now)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, mapError(err)
	}

	return released, nil
}

func releaseExpired(ctx context.Context, tx pgx.Tx, now time.Time) (int, error) {
	// The branch is freed with the task, in the same transaction. A worker that
	// died holding a lease must not keep its branch: that is the whole reason
	// leases expire, and leaving the row behind would turn a crashed worker
	// into a permanently blocked branch.
	if _, err := tx.Exec(ctx, `
		DELETE FROM partition_leases
		WHERE task_id IN (
			SELECT id FROM tasks WHERE state = 'running' AND lease_expires_at <= $1
		)`, now); err != nil {
		return 0, mapError(err)
	}

	tag, err := tx.Exec(ctx, `
		UPDATE tasks
		SET state = 'queued',
		    lease_holder = NULL, lease_token = NULL, lease_expires_at = NULL,
		    started_at = NULL,
		    updated_at = $1
		WHERE state = 'running' AND lease_expires_at <= $1`, now)
	if err != nil {
		return 0, mapError(err)
	}

	return int(tag.RowsAffected()), nil
}

// QueuePosition counts what stands between a task and execution: tasks already
// running in its partition, plus queued ones that arrived earlier.
func (s *taskStore) QueuePosition(ctx context.Context, id store.ID) (int, error) {
	var (
		state        protocol.TaskState
		partitionKey *string
		createdAt    time.Time
	)

	err := s.pool.QueryRow(ctx,
		`SELECT state, partition_key, created_at FROM tasks WHERE id = $1::uuid`,
		string(id)).Scan(&state, &partitionKey, &createdAt)
	if err != nil {
		return 0, mapError(err)
	}

	if state != protocol.TaskQueued || partitionKey == nil {
		return 0, nil
	}

	var position int

	err = s.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM tasks
		WHERE partition_key = $1
		  AND id <> $2::uuid
		  AND (
		      state = 'running'
		      OR (state = 'queued' AND (created_at < $3 OR (created_at = $3 AND id::text < $2::text)))
		  )`,
		*partitionKey, string(id), createdAt).Scan(&position)
	if err != nil {
		return 0, mapError(err)
	}

	return position, nil
}

func (s *taskStore) Cancel(ctx context.Context, id store.ID, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks
		SET state = 'cancelled', updated_at = $2, finished_at = $2
		WHERE id = $1::uuid AND state = 'queued'`,
		string(id), at)
	if err != nil {
		return mapError(err)
	}

	if tag.RowsAffected() == 1 {
		return nil
	}

	// Nothing changed: either the task is gone, or it is no longer queued. A
	// running task belongs to its worker and cancelling the record underneath
	// it would leave the forge in a state nobody can describe.
	var exists bool
	if err := s.pool.QueryRow(ctx,
		`SELECT true FROM tasks WHERE id = $1::uuid`, string(id)).Scan(&exists); err != nil {
		return mapError(err)
	}

	return store.ErrConflict
}

// leaseResult distinguishes "the task does not exist" from "someone else owns
// it". A worker must be able to tell those apart: the first is a bug, the
// second is a lost race it has to abandon.
func leaseResult(ctx context.Context, pool *pgxpool.Pool, id store.ID, affected int64) error {
	if affected == 1 {
		return nil
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT true FROM tasks WHERE id = $1::uuid`, string(id)).Scan(&exists); err != nil {
		return mapError(err)
	}

	return store.ErrLeaseLost
}

// stateArray and kindArray map an empty filter to NULL, which the queries read
// as "no restriction".
func stateArray(states []protocol.TaskState) any {
	if len(states) == 0 {
		return nil
	}

	out := make([]string, len(states))
	for i, s := range states {
		out[i] = string(s)
	}
	return out
}

func kindArray(kinds []protocol.TaskKind) any {
	if len(kinds) == 0 {
		return nil
	}

	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = string(k)
	}
	return out
}

var _ store.TaskStore = (*taskStore)(nil)
