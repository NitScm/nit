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

// claimLockKey is the advisory lock every claim takes. It is an arbitrary but
// fixed value: all nit processes sharing a database must agree on it.
const claimLockKey = 0x6E697402 // "nit\x02"

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
// FOR UPDATE SKIP LOCKED excludes workers competing for the same *row*. It does
// nothing for two workers picking two *different* rows of the same branch: under
// READ COMMITTED neither transaction sees the other's uncommitted UPDATE, so
// both pass the "is this branch busy?" test and both start pushing to the same
// branch. A first implementation had exactly that bug; it passed every
// sequential test and failed the concurrent one immediately.
//
// The fix is an advisory lock that serializes the claim itself. Two designs
// were possible:
//
//   - one lock per partition, keeping claims on different branches concurrent;
//   - one lock for the whole claim, serializing claims but nothing else.
//
// The per-partition version is taken here as the transaction scans candidate
// rows, which means a worker can hold a lock on a branch it does not end up
// claiming and block a worker that would have. That trades a correctness
// problem for a liveness one.
//
// The global lock is used instead. A claim is a single indexed query and a
// short UPDATE; serializing it costs microseconds, while the work it dispatches
// — clone, apply, push, minutes at a time — stays fully parallel. If the claim
// rate ever becomes the bottleneck, the per-partition scheme is the escape
// hatch, and it needs the candidate set narrowed to one row before the lock is
// taken.
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

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	defer tx.Rollback(ctx)

	// Serializes the claim, and only the claim: released at commit, well before
	// the dispatched task does any work.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, claimLockKey); err != nil {
		return nil, mapError(err)
	}

	if _, err := releaseExpired(ctx, tx, now); err != nil {
		return nil, err
	}

	task, err := scanTask(tx.QueryRow(ctx, `
		WITH next AS (
			SELECT t.id
			FROM tasks t
			WHERE t.state = 'queued'
			  AND ($1::task_kind[] IS NULL OR t.kind = ANY($1::task_kind[]))
			  AND (
			      t.partition_key IS NULL
			      OR NOT EXISTS (
			          SELECT 1 FROM tasks busy
			          WHERE busy.partition_key = t.partition_key
			            AND busy.state = 'running'
			      )
			  )
			ORDER BY t.created_at, t.id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE tasks
		SET state = 'running',
		    attempts = attempts + 1,
		    lease_holder = $2,
		    lease_token = gen_random_uuid()::text,
		    lease_expires_at = $3,
		    started_at = COALESCE(started_at, $4),
		    updated_at = $4
		WHERE id IN (SELECT id FROM next)
		RETURNING `+taskColumns,
		kindArray(opts.Kinds), opts.Holder, now.Add(leaseFor), now))

	if errors.Is(err, store.ErrNotFound) {
		return nil, store.ErrNoTask
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

	tag, err := s.pool.Exec(ctx, `
		UPDATE tasks
		SET state = 'succeeded',
		    result = $3,
		    error = NULL,
		    lease_holder = NULL, lease_token = NULL, lease_expires_at = NULL,
		    updated_at = $4, finished_at = $4
		WHERE id = $1::uuid AND state = 'running' AND lease_token = $2`,
		string(id), token, result, at)
	if err != nil {
		return mapError(err)
	}

	return leaseResult(ctx, s.pool, id, tag.RowsAffected())
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

	tag, err := s.pool.Exec(ctx, query, string(id), token, causeJSON, at)
	if err != nil {
		return mapError(err)
	}

	return leaseResult(ctx, s.pool, id, tag.RowsAffected())
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
