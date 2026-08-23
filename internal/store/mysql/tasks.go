package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// claimLock is the name every claim serializes on.
//
// Scoped to the database rather than fixed, because GET_LOCK names are
// server-wide: two nit databases on one MySQL server would otherwise serialize
// each other's queues for no reason, and the symptom would be a slow queue with
// nothing in the slow query log.
const claimLock = `CONCAT(DATABASE(), ':nit:claim')`

// lockWait is how long a claim waits for the lock before giving up. Long enough
// that a busy queue does not spuriously report an empty one, short enough that
// a wedged session is visible rather than hanging a worker forever.
const lockWait = 10

type taskStore struct{ db *sql.DB }

const taskColumns = `
	id, tenant_id, request_id, kind, state,
	user_id, workspace_id, repository_id, branch, partition_key,
	payload, result, error, attempts,
	lease_holder, lease_token, lease_expires_at,
	created_at, updated_at, started_at, finished_at`

func scanTask(row scanner) (*store.Task, error) {
	var (
		t            store.Task
		id           string
		tenantID     string
		kind         string
		state        string
		userID       string
		repoID       string
		workspaceID  sql.NullString
		partitionKey sql.NullString
		result       []byte
		errorJSON    []byte
		leaseHolder  sql.NullString
		leaseToken   sql.NullString
		leaseExpires sql.NullTime
		created      sql.NullTime
		updated      sql.NullTime
		started      sql.NullTime
		finished     sql.NullTime
	)

	err := row.Scan(
		&id, &tenantID, &t.RequestID, &kind, &state,
		&userID, &workspaceID, &repoID, &t.Branch, &partitionKey,
		&t.Payload, &result, &errorJSON, &t.Attempts,
		&leaseHolder, &leaseToken, &leaseExpires,
		&created, &updated, &started, &finished,
	)
	if err != nil {
		return nil, mapError(err)
	}

	t.ID = store.ID(id)
	t.TenantID = policy.TenantID(tenantID)
	t.Kind = protocol.TaskKind(kind)
	t.State = protocol.TaskState(state)
	t.UserID = store.ID(userID)
	t.RepositoryID = store.ID(repoID)
	t.WorkspaceID = textID(workspaceID)
	t.PartitionKey = text(partitionKey)
	t.Result = result
	t.CreatedAt = deref(created)
	t.UpdatedAt = deref(updated)
	t.StartedAt = timePtr(started)
	t.FinishedAt = timePtr(finished)

	if len(errorJSON) > 0 {
		var cause protocol.Error
		if err := json.Unmarshal(errorJSON, &cause); err != nil {
			return nil, err
		}
		t.Error = &cause
	}

	// The lease columns are constrained to move together (see
	// tasks_lease_consistency), so testing one is enough.
	if leaseToken.Valid {
		t.Lease = &store.Lease{
			Holder:    text(leaseHolder),
			Token:     leaseToken.String,
			ExpiresAt: deref(leaseExpires),
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

	id := t.ID
	if id == "" {
		id = newID()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO tasks (
			id, tenant_id, request_id, kind, state,
			user_id, workspace_id, repository_id, branch, partition_key,
			payload, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(id), string(t.TenantID), t.RequestID, string(t.Kind), string(state),
		string(t.UserID), nullableID(t.WorkspaceID), string(t.RepositoryID), t.Branch, nullable(t.PartitionKey),
		payload, createdAt.UTC(), createdAt.UTC())

	// A repeated request id is not an error the caller has to reason about
	// twice: hand back the task that already exists, together with the
	// sentinel, exactly as the other stores do.
	if err != nil && isDuplicate(err, "tasks_request_id_unique") {
		existing, lookupErr := s.ByRequestID(ctx, t.TenantID, t.RequestID)
		if lookupErr != nil {
			return nil, store.ErrDuplicateRequest
		}
		return existing, store.ErrDuplicateRequest
	}
	if err != nil {
		return nil, mapError(err)
	}

	return s.ByID(ctx, id)
}

func (s *taskStore) ByID(ctx context.Context, id store.ID) (*store.Task, error) {
	return scanTask(s.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, string(id)))
}

func (s *taskStore) ByRequestID(ctx context.Context, tenant policy.TenantID, requestID string) (*store.Task, error) {
	return scanTask(s.db.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE tenant_id = ? AND request_id = ?`,
		string(tenant), requestID))
}

func (s *taskStore) List(ctx context.Context, f store.TaskFilter) ([]*store.Task, error) {
	limit := f.Limit
	if limit <= 0 {
		limit = 100
	}

	where := []string{"1 = 1"}
	args := []any{}

	if f.Tenant != "" {
		where = append(where, "tenant_id = ?")
		args = append(args, string(f.Tenant))
	}
	if f.UserID != "" {
		where = append(where, "user_id = ?")
		args = append(args, string(f.UserID))
	}
	if f.RepositoryID != "" {
		where = append(where, "repository_id = ?")
		args = append(args, string(f.RepositoryID))
	}
	if f.Branch != "" {
		where = append(where, "branch = ?")
		args = append(args, f.Branch)
	}
	if len(f.States) > 0 {
		where = append(where, "state IN ("+inPlaceholders(len(f.States))+")")
		for _, state := range f.States {
			args = append(args, string(state))
		}
	}
	if len(f.Kinds) > 0 {
		where = append(where, "kind IN ("+inPlaceholders(len(f.Kinds))+")")
		for _, kind := range f.Kinds {
			args = append(args, string(kind))
		}
	}

	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+taskColumns+`
		FROM tasks
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY created_at DESC, id DESC
		LIMIT ?`, args...)
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
// The correctness argument is the one in the PostgreSQL implementation, and it
// is worth reading there first: SKIP LOCKED excludes workers competing for the
// same *row* and does nothing for two workers picking two *different* rows of
// the same branch, so a lock around the claim itself is what makes partition
// exclusion hold. A global lock is used rather than one per partition, because
// serializing a short indexed query costs microseconds while the work it
// dispatches stays fully parallel.
//
// Three things differ here, and each is forced by the engine.
//
// The lock is GET_LOCK, which is held by the *session*, not the transaction.
// PostgreSQL's pg_advisory_xact_lock is released by the commit itself, in the
// right order, for free. Here the order has to be arranged by hand, and getting
// it wrong is not a slow queue but a double claim — see below.
//
// The claim runs on a dedicated connection. database/sql hands a transaction a
// connection and takes it back at commit, so a lock taken on "the connection"
// outside the transaction has no way to be released on the same one afterwards.
//
// The transaction is READ COMMITTED, not MySQL's REPEATABLE READ default. Under
// REPEATABLE READ every statement in the claim reads the snapshot taken at the
// first one, so the busy-partition test could be answered from a view of the
// world that predates a task another worker started.
func (s *taskStore) Claim(ctx context.Context, opts store.ClaimOptions) (*store.Task, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}

	leaseFor := opts.LeaseFor
	if leaseFor <= 0 {
		leaseFor = time.Minute
	}

	conn, err := s.db.Conn(ctx)
	if err != nil {
		return nil, mapError(err)
	}
	defer conn.Close()

	if err := acquireLock(ctx, conn); err != nil {
		return nil, err
	}

	// Deliberately not deferred alongside the commit: the lock must outlive it.
	// If it were released first, another session could take the lock and open
	// its transaction before this one's UPDATE became visible — and would then
	// find the partition idle and claim a second task on the same branch. That
	// is the bug the lock exists to prevent, reintroduced by the order of two
	// lines.
	defer releaseLock(context.WithoutCancel(ctx), conn)

	var task *store.Task

	err = retryOnDeadlock(func() error {
		claimed, err := s.claimLocked(ctx, conn, opts, now, leaseFor)
		if err != nil {
			return err
		}

		task = claimed

		return nil
	})
	if err != nil {
		return nil, err
	}

	return task, nil
}

func (s *taskStore) claimLocked(ctx context.Context, conn *sql.Conn, opts store.ClaimOptions, now time.Time, leaseFor time.Duration) (*store.Task, error) {
	tx, err := conn.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return nil, mapError(err)
	}
	defer tx.Rollback()

	if _, err := releaseExpired(ctx, tx, now); err != nil {
		return nil, err
	}

	// A plain SELECT, with no locking clause. FOR UPDATE SKIP LOCKED would be
	// the direct translation, but MySQL applies the locking clause to every
	// table in the statement including the busy-partition subquery, and
	// MariaDB has no FOR UPDATE OF to narrow it. Skipping a locked *running*
	// row would make the partition look idle. Under the claim lock no other
	// claim can interleave, so no row lock is needed to get this right.
	where := []string{"t.state = 'queued'"}
	args := []any{}

	if len(opts.Kinds) > 0 {
		where = append(where, "t.kind IN ("+inPlaceholders(len(opts.Kinds))+")")
		for _, kind := range opts.Kinds {
			args = append(args, string(kind))
		}
	}

	var id string

	err = tx.QueryRowContext(ctx, `
		SELECT t.id
		FROM tasks t
		WHERE `+strings.Join(where, " AND ")+`
		  AND (
		      t.partition_key IS NULL
		      OR NOT EXISTS (
		          SELECT 1 FROM tasks busy
		          WHERE busy.partition_key = t.partition_key
		            AND busy.state = 'running'
		      )
		  )
		ORDER BY t.created_at, t.id
		LIMIT 1`, args...).Scan(&id)

	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNoTask
	}
	if err != nil {
		return nil, mapError(err)
	}

	result, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET state = 'running',
		    attempts = attempts + 1,
		    lease_holder = ?,
		    lease_token = ?,
		    lease_expires_at = ?,
		    started_at = COALESCE(started_at, ?),
		    updated_at = ?
		WHERE id = ? AND state = 'queued'`,
		opts.Holder, string(newID()), now.Add(leaseFor).UTC(), now.UTC(), now.UTC(), id)
	if err != nil {
		return nil, mapError(err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return nil, mapError(err)
	}
	if affected == 0 {
		// Nothing else can have taken it while the lock is held, so this means
		// the row left the queue by another route — cancelled, most likely.
		return nil, store.ErrNoTask
	}

	task, err := scanTask(tx.QueryRowContext(ctx,
		`SELECT `+taskColumns+` FROM tasks WHERE id = ?`, id))
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, mapError(err)
	}

	return task, nil
}

// acquireLock takes the claim lock, distinguishing a timeout from a failure.
func acquireLock(ctx context.Context, conn *sql.Conn) error {
	var got sql.NullInt64

	if err := conn.QueryRowContext(ctx,
		`SELECT GET_LOCK(`+claimLock+`, ?)`, lockWait).Scan(&got); err != nil {
		return mapError(err)
	}

	if !got.Valid {
		return fmt.Errorf("mysql: claim lock: the server refused the request")
	}
	if got.Int64 != 1 {
		return fmt.Errorf("mysql: claim lock: not acquired within %ds; another session is holding it", lockWait)
	}

	return nil
}

func releaseLock(ctx context.Context, conn *sql.Conn) {
	// The result is deliberately ignored: the lock is released by the session
	// ending in any case, and a failure here has no recovery a caller could
	// perform.
	_, _ = conn.ExecContext(ctx, `DO RELEASE_LOCK(`+claimLock+`)`)
}

func (s *taskStore) Heartbeat(ctx context.Context, id store.ID, token string, until time.Time) error {
	return retryOnDeadlock(func() error { return s.heartbeat(ctx, id, token, until) })
}

func (s *taskStore) heartbeat(ctx context.Context, id store.ID, token string, until time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET lease_expires_at = ?, updated_at = ?
		WHERE id = ? AND state = 'running' AND lease_token = ?`,
		until.UTC(), until.UTC(), string(id), token)
	if err != nil {
		return mapError(err)
	}

	return s.leaseResult(ctx, id, result)
}

func (s *taskStore) Complete(ctx context.Context, id store.ID, token string, result []byte, at time.Time) error {
	return retryOnDeadlock(func() error { return s.complete(ctx, id, token, result, at) })
}

func (s *taskStore) complete(ctx context.Context, id store.ID, token string, result []byte, at time.Time) error {
	if len(result) == 0 {
		result = []byte(`{}`)
	}

	updated, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET state = 'succeeded',
		    result = ?,
		    error = NULL,
		    lease_holder = NULL, lease_token = NULL, lease_expires_at = NULL,
		    updated_at = ?, finished_at = ?
		WHERE id = ? AND state = 'running' AND lease_token = ?`,
		result, at.UTC(), at.UTC(), string(id), token)
	if err != nil {
		return mapError(err)
	}

	return s.leaseResult(ctx, id, updated)
}

func (s *taskStore) Fail(ctx context.Context, id store.ID, token string, cause *protocol.Error, requeue bool, at time.Time) error {
	return retryOnDeadlock(func() error { return s.fail(ctx, id, token, cause, requeue, at) })
}

func (s *taskStore) fail(ctx context.Context, id store.ID, token string, cause *protocol.Error, requeue bool, at time.Time) error {
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
		    error = ?,
		    lease_holder = NULL, lease_token = NULL, lease_expires_at = NULL,
		    updated_at = ?, finished_at = ?
		WHERE id = ? AND state = 'running' AND lease_token = ?`

	if requeue {
		query = `
			UPDATE tasks
			SET state = 'queued',
			    error = ?,
			    lease_holder = NULL, lease_token = NULL, lease_expires_at = NULL,
			    started_at = NULL,
			    updated_at = ?, finished_at = NULL
			WHERE id = ? AND state = 'running' AND lease_token = ?`
	}

	var (
		updated sql.Result
		err     error
	)

	if requeue {
		updated, err = s.db.ExecContext(ctx, query, nullJSON(causeJSON), at.UTC(), string(id), token)
	} else {
		updated, err = s.db.ExecContext(ctx, query, nullJSON(causeJSON), at.UTC(), at.UTC(), string(id), token)
	}
	if err != nil {
		return mapError(err)
	}

	return s.leaseResult(ctx, id, updated)
}

func (s *taskStore) ReleaseExpired(ctx context.Context, now time.Time) (int, error) {
	var released int

	err := retryOnDeadlock(func() error {
		count, err := s.releaseExpiredTx(ctx, now)
		released = count

		return err
	})

	return released, err
}

func (s *taskStore) releaseExpiredTx(ctx context.Context, now time.Time) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, mapError(err)
	}
	defer tx.Rollback()

	released, err := releaseExpired(ctx, tx, now)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(); err != nil {
		return 0, mapError(err)
	}

	return released, nil
}

// releaseExpired returns lapsed leases to the queue.
//
// It reads the ids first and updates by primary key, rather than issuing one
// UPDATE with the same predicate. The direct form deadlocks against Complete
// under load, and the reason is worth keeping: a scan of
// idx_tasks_lease_expiry takes the secondary index lock before the row lock,
// while Complete — a lookup by id — takes them the other way round. Two
// transactions, two orders, one cycle. Reproduced with 12 workers draining 30
// tasks; InnoDB reported "Deadlock found when trying to get lock" and three
// tasks were never completed.
//
// Updating by id makes both paths lock the primary key first. The predicate is
// repeated in the UPDATE because the rows may have moved between the two
// statements — the read is not locking, deliberately, since holding read locks
// across the claim is what the claim lock already prevents the need for.
func releaseExpired(ctx context.Context, tx *sql.Tx, now time.Time) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id FROM tasks
		WHERE state = 'running' AND lease_expires_at <= ?
		ORDER BY id`, now.UTC())
	if err != nil {
		return 0, mapError(err)
	}

	var ids []any

	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, mapError(err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	if err := rows.Err(); err != nil {
		return 0, mapError(err)
	}
	if len(ids) == 0 {
		return 0, nil
	}

	args := append([]any{now.UTC()}, ids...)
	args = append(args, now.UTC())

	result, err := tx.ExecContext(ctx, `
		UPDATE tasks
		SET state = 'queued',
		    lease_holder = NULL, lease_token = NULL, lease_expires_at = NULL,
		    started_at = NULL,
		    updated_at = ?
		WHERE id IN (`+inPlaceholders(len(ids))+`)
		  AND state = 'running' AND lease_expires_at <= ?`, args...)
	if err != nil {
		return 0, mapError(err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, mapError(err)
	}

	return int(affected), nil
}

// retryOnDeadlock runs fn again when InnoDB picks it as a deadlock victim.
//
// A deadlock is not a failure a caller can act on: the engine rolled the
// transaction back completely, so nothing happened and the operation can simply
// be repeated. Surfacing it would make every worker implement this loop, and
// the store is where the backend's quirks belong.
//
// Only error 1213 is retried. A lock wait timeout (1205) rolls back the
// *statement* and leaves the transaction open, so repeating it blind could
// apply half an operation twice; that one is reported.
func retryOnDeadlock(fn func() error) error {
	const attempts = 5

	var err error

	for attempt := range attempts {
		err = fn()
		if !isDeadlock(err) {
			return err
		}

		// A short, growing pause. Retrying instantly tends to reproduce the
		// same interleaving that deadlocked.
		time.Sleep(time.Duration(attempt+1) * time.Millisecond)
	}

	return err
}

func isDeadlock(err error) bool {
	var mysqlErr *mysql.MySQLError

	return errors.As(err, &mysqlErr) && mysqlErr.Number == 1213
}

// QueuePosition counts what stands between a task and execution: tasks already
// running in its partition, plus queued ones that arrived earlier.
func (s *taskStore) QueuePosition(ctx context.Context, id store.ID) (int, error) {
	var (
		state        string
		partitionKey sql.NullString
		createdAt    sql.NullTime
	)

	err := s.db.QueryRowContext(ctx,
		`SELECT state, partition_key, created_at FROM tasks WHERE id = ?`,
		string(id)).Scan(&state, &partitionKey, &createdAt)
	if err != nil {
		return 0, mapError(err)
	}

	if protocol.TaskState(state) != protocol.TaskQueued || !partitionKey.Valid {
		return 0, nil
	}

	var position int

	err = s.db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM tasks
		WHERE partition_key = ?
		  AND id <> ?
		  AND (
		      state = 'running'
		      OR (state = 'queued' AND (created_at < ? OR (created_at = ? AND id < ?)))
		  )`,
		partitionKey.String, string(id), deref(createdAt), deref(createdAt), string(id)).Scan(&position)
	if err != nil {
		return 0, mapError(err)
	}

	return position, nil
}

func (s *taskStore) Cancel(ctx context.Context, id store.ID, at time.Time) error {
	return retryOnDeadlock(func() error { return s.cancel(ctx, id, at) })
}

func (s *taskStore) cancel(ctx context.Context, id store.ID, at time.Time) error {
	result, err := s.db.ExecContext(ctx, `
		UPDATE tasks
		SET state = 'cancelled', updated_at = ?, finished_at = ?
		WHERE id = ? AND state = 'queued'`,
		at.UTC(), at.UTC(), string(id))
	if err != nil {
		return mapError(err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return mapError(err)
	}
	if affected == 1 {
		return nil
	}

	// Nothing matched: either the task is gone, or it is no longer queued. A
	// running task belongs to its worker and cancelling the record underneath
	// it would leave the forge in a state nobody can describe.
	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT TRUE FROM tasks WHERE id = ?`, string(id)).Scan(&exists); err != nil {
		return mapError(err)
	}

	return store.ErrConflict
}

// leaseResult distinguishes "the task does not exist" from "someone else owns
// it". A worker must be able to tell those apart: the first is a bug, the
// second is a lost race it has to abandon.
//
// This reads *matched* rows rather than changed ones — the DSN sets
// clientFoundRows, without which a heartbeat that extended a lease to the
// instant it already held would report zero and be mistaken for a lost lease.
func (s *taskStore) leaseResult(ctx context.Context, id store.ID, result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return mapError(err)
	}
	if affected == 1 {
		return nil
	}

	var exists bool
	if err := s.db.QueryRowContext(ctx,
		`SELECT TRUE FROM tasks WHERE id = ?`, string(id)).Scan(&exists); err != nil {
		return mapError(err)
	}

	return store.ErrLeaseLost
}

var _ store.TaskStore = (*taskStore)(nil)
