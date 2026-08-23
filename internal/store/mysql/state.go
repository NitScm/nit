package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// ---------------------------------------------------------------------------
// sessions
// ---------------------------------------------------------------------------

type sessionStore struct{ db *sql.DB }

const sessionColumns = `id, tenant_id, user_id, token_hash, label, created_at, expires_at, last_used_at, revoked_at`

func scanSession(row scanner) (*store.Session, error) {
	var (
		s        store.Session
		id       string
		tenantID string
		userID   string
		created  sql.NullTime
		expires  sql.NullTime
		lastUsed sql.NullTime
		revoked  sql.NullTime
	)

	err := row.Scan(&id, &tenantID, &userID, &s.TokenHash, &s.Label,
		&created, &expires, &lastUsed, &revoked)
	if err != nil {
		return nil, mapError(err)
	}

	s.ID = store.ID(id)
	s.TenantID = policy.TenantID(tenantID)
	s.UserID = store.ID(userID)
	s.CreatedAt = deref(created)
	s.ExpiresAt = timePtr(expires)
	s.LastUsedAt = timePtr(lastUsed)
	s.RevokedAt = timePtr(revoked)

	return &s, nil
}

func (s *sessionStore) Create(ctx context.Context, sess *store.Session) (*store.Session, error) {
	id := newID()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (id, tenant_id, user_id, token_hash, label, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		string(id), string(sess.TenantID), string(sess.UserID),
		sess.TokenHash, sess.Label, nullTime(sess.ExpiresAt))
	if err != nil {
		return nil, mapError(err)
	}

	return s.ByID(ctx, id)
}

// ByTokenHash finds a session by the hash of its token.
//
// Revoked and expired sessions are returned too. The caller decides what to do
// with them, so that an expired token can be reported as expired instead of
// looking like a token that never existed — the difference between "log in
// again" and "something is very wrong".
func (s *sessionStore) ByTokenHash(ctx context.Context, tenant policy.TenantID, hash []byte) (*store.Session, error) {
	return scanSession(s.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE tenant_id = ? AND token_hash = ?`,
		string(tenant), hash))
}

func (s *sessionStore) ByID(ctx context.Context, id store.ID) (*store.Session, error) {
	return scanSession(s.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, string(id)))
}

func (s *sessionStore) ListByUser(ctx context.Context, userID store.ID) ([]*store.Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE user_id = ? ORDER BY created_at, id`,
		string(userID))
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []*store.Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}

	return out, mapError(rows.Err())
}

func (s *sessionStore) Touch(ctx context.Context, id store.ID, at time.Time) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_used_at = ? WHERE id = ?`, at.UTC(), string(id))
	if err != nil {
		return mapError(err)
	}

	return requireRow(result)
}

// Revoke marks a session unusable. A second call keeps the first instant: for
// an incident timeline, when a credential was first cut off is the fact that
// matters.
//
// A second call therefore changes nothing, and still has to succeed. That works
// only because the DSN sets clientFoundRows: requireRow reads matched rows, so
// an update that rewrites nothing is still one row.
func (s *sessionStore) Revoke(ctx context.Context, id store.ID, at time.Time) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`,
		at.UTC(), string(id))
	if err != nil {
		return mapError(err)
	}

	return requireRow(result)
}

// ---------------------------------------------------------------------------
// sync points
// ---------------------------------------------------------------------------

type syncPointStore struct{ db *sql.DB }

const syncPointColumns = `tenant_id, workspace_id, repository_id, branch, upstream_commit, policy_version, created_at, updated_at`

func scanSyncPoint(row scanner) (*store.SyncPoint, error) {
	var (
		sp          store.SyncPoint
		tenantID    string
		workspaceID string
		repoID      string
		created     sql.NullTime
		updated     sql.NullTime
	)

	err := row.Scan(&tenantID, &workspaceID, &repoID, &sp.Branch,
		&sp.UpstreamCommit, &sp.PolicyVersion, &created, &updated)
	if err != nil {
		return nil, mapError(err)
	}

	sp.TenantID = policy.TenantID(tenantID)
	sp.WorkspaceID = store.ID(workspaceID)
	sp.RepositoryID = store.ID(repoID)
	sp.CreatedAt = deref(created)
	sp.UpdatedAt = deref(updated)

	return &sp, nil
}

func (s *syncPointStore) Get(ctx context.Context, workspaceID, repositoryID store.ID, branch string) (*store.SyncPoint, error) {
	return scanSyncPoint(s.db.QueryRowContext(ctx, `
		SELECT `+syncPointColumns+`
		FROM sync_points
		WHERE workspace_id = ? AND repository_id = ? AND branch = ?`,
		string(workspaceID), string(repositoryID), branch))
}

func (s *syncPointStore) Put(ctx context.Context, sp *store.SyncPoint) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sync_points (tenant_id, workspace_id, repository_id, branch, upstream_commit, policy_version)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			upstream_commit = ?,
			policy_version = ?,
			updated_at = CURRENT_TIMESTAMP(6)`,
		string(sp.TenantID), string(sp.WorkspaceID), string(sp.RepositoryID),
		sp.Branch, sp.UpstreamCommit, sp.PolicyVersion,
		sp.UpstreamCommit, sp.PolicyVersion)

	return mapError(err)
}

// CompareAndSet advances a sync point only if it still holds the expected
// commit.
//
// Without this, two operations on the same workspace and branch could
// interleave and leave the client believing it is projected from a commit it
// never received — after which every subsequent diff is computed against the
// wrong base.
func (s *syncPointStore) CompareAndSet(ctx context.Context, sp *store.SyncPoint, expectedCommit string) error {
	if expectedCommit == "" {
		// Creating a sync point where none existed. INSERT IGNORE makes the
		// insert lose the race rather than overwrite a point another operation
		// established in the meantime.
		//
		// IGNORE downgrades more than duplicate keys — a bad foreign key would
		// also become a warning — so the caller is told "conflict" for a
		// condition that might be something else. The alternative, an
		// ON DUPLICATE KEY UPDATE that assigns a column to itself, reports one
		// affected row for a *collision*, which loses the distinction that
		// matters here.
		result, err := s.db.ExecContext(ctx, `
			INSERT IGNORE INTO sync_points
				(tenant_id, workspace_id, repository_id, branch, upstream_commit, policy_version)
			VALUES (?, ?, ?, ?, ?, ?)`,
			string(sp.TenantID), string(sp.WorkspaceID), string(sp.RepositoryID),
			sp.Branch, sp.UpstreamCommit, sp.PolicyVersion)
		if err != nil {
			return mapError(err)
		}

		affected, err := result.RowsAffected()
		if err != nil {
			return mapError(err)
		}
		if affected == 0 {
			return store.ErrConflict
		}

		return nil
	}

	result, err := s.db.ExecContext(ctx, `
		UPDATE sync_points
		SET upstream_commit = ?, policy_version = ?, updated_at = CURRENT_TIMESTAMP(6)
		WHERE workspace_id = ? AND repository_id = ? AND branch = ?
		  AND upstream_commit = ?`,
		sp.UpstreamCommit, sp.PolicyVersion,
		string(sp.WorkspaceID), string(sp.RepositoryID), sp.Branch, expectedCommit)
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

	// Nothing matched, so the expected commit no longer holds. Not "nothing
	// changed": clientFoundRows counts matched rows, so advancing a sync point
	// to the value it already carries still reports one — the same answer
	// PostgreSQL gives, where an UPDATE writes the row either way.
	return store.ErrConflict
}

func (s *syncPointStore) ListByWorkspace(ctx context.Context, workspaceID store.ID) ([]*store.SyncPoint, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+syncPointColumns+`
		FROM sync_points
		WHERE workspace_id = ?
		ORDER BY repository_id, branch`,
		string(workspaceID))
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []*store.SyncPoint
	for rows.Next() {
		sp, err := scanSyncPoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sp)
	}

	return out, mapError(rows.Err())
}

func (s *syncPointStore) Delete(ctx context.Context, workspaceID, repositoryID store.ID, branch string) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM sync_points
		WHERE workspace_id = ? AND repository_id = ? AND branch = ?`,
		string(workspaceID), string(repositoryID), branch)

	return mapError(err)
}

// ---------------------------------------------------------------------------
// artifacts
// ---------------------------------------------------------------------------

type artifactStore struct{ db *sql.DB }

const artifactColumns = `id, tenant_id, digest, kind, size_bytes, uncompressed_bytes, encoding, locator, created_at, expires_at`

func scanArtifact(row scanner) (*store.Artifact, error) {
	var (
		a        store.Artifact
		id       string
		tenantID string
		kind     string
		encoding string
		created  sql.NullTime
		expires  sql.NullTime
	)

	err := row.Scan(&id, &tenantID, &a.Digest, &kind, &a.Size,
		&a.UncompressedSize, &encoding, &a.Locator, &created, &expires)
	if err != nil {
		return nil, mapError(err)
	}

	a.ID = store.ID(id)
	a.TenantID = policy.TenantID(tenantID)
	a.Kind = store.ArtifactKind(kind)
	a.Encoding = protocol.Encoding(encoding)
	a.CreatedAt = deref(created)
	a.ExpiresAt = timePtr(expires)

	return &a, nil
}

func (s *artifactStore) ByDigest(ctx context.Context, tenant policy.TenantID, digest string) (*store.Artifact, error) {
	return scanArtifact(s.db.QueryRowContext(ctx,
		`SELECT `+artifactColumns+` FROM artifacts WHERE tenant_id = ? AND digest = ?`,
		string(tenant), digest))
}

// Create records a blob, returning the existing record for identical bytes:
// content addressing means the same digest is the same artifact, and storing it
// twice would only create two rows pointing at one file.
func (s *artifactStore) Create(ctx context.Context, a *store.Artifact) (*store.Artifact, error) {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO artifacts
			(id, tenant_id, digest, kind, size_bytes, uncompressed_bytes, encoding, locator, expires_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP(6)))
		ON DUPLICATE KEY UPDATE
			expires_at = COALESCE(?, expires_at)`,
		string(newID()), string(a.TenantID), a.Digest, string(a.Kind), a.Size,
		a.UncompressedSize, string(a.Encoding), a.Locator,
		nullTime(a.ExpiresAt), nullInstant(a.CreatedAt),
		nullTime(a.ExpiresAt))
	if err != nil {
		return nil, mapError(err)
	}

	return s.ByDigest(ctx, a.TenantID, a.Digest)
}

func (s *artifactStore) Expired(ctx context.Context, now time.Time, limit int) ([]*store.Artifact, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+artifactColumns+`
		FROM artifacts
		WHERE expires_at IS NOT NULL AND expires_at <= ?
		ORDER BY expires_at
		LIMIT ?`, now.UTC(), limit)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []*store.Artifact
	for rows.Next() {
		a, err := scanArtifact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}

	return out, mapError(rows.Err())
}

func (s *artifactStore) Delete(ctx context.Context, id store.ID) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM artifacts WHERE id = ?`, string(id))
	return mapError(err)
}

// ---------------------------------------------------------------------------
// audit
// ---------------------------------------------------------------------------

type auditStore struct{ db *sql.DB }

const auditColumns = `
	id, tenant_id, occurred_at,
	actor_user_id, actor_label, action,
	repository_id, branch, path,
	effect, reason, rule_id, guard,
	policy_version, request_id, task_id, detail`

func scanAudit(row scanner) (*store.AuditRecord, error) {
	var (
		r            store.AuditRecord
		tenantID     string
		occurred     sql.NullTime
		actorUserID  sql.NullString
		repositoryID sql.NullString
		branch       sql.NullString
		path         sql.NullString
		effect       sql.NullString
		reason       sql.NullString
		ruleID       sql.NullString
		guard        sql.NullString
		requestID    sql.NullString
		taskID       sql.NullString
	)

	err := row.Scan(
		&r.ID, &tenantID, &occurred,
		&actorUserID, &r.ActorLabel, &r.Action,
		&repositoryID, &branch, &path,
		&effect, &reason, &ruleID, &guard,
		&r.PolicyVersion, &requestID, &taskID, &r.Detail,
	)
	if err != nil {
		return nil, mapError(err)
	}

	r.TenantID = policy.TenantID(tenantID)
	r.OccurredAt = deref(occurred)
	r.ActorUserID = textID(actorUserID)
	r.RepositoryID = textID(repositoryID)
	r.Branch = text(branch)
	r.Path = text(path)
	r.Effect = policy.Effect(text(effect))
	r.Reason = policy.Reason(text(reason))
	r.RuleID = text(ruleID)
	r.Guard = text(guard)
	r.RequestID = text(requestID)
	r.TaskID = textID(taskID)

	return &r, nil
}

// Append writes every record in one statement.
//
// The table refuses UPDATE and DELETE with a trigger, so a bug in the
// application cannot rewrite history even if it tries. TRUNCATE is the gap this
// backend cannot close in SQL; migration 0002 explains what closes it instead.
func (s *auditStore) Append(ctx context.Context, records ...*store.AuditRecord) error {
	if len(records) == 0 {
		return nil
	}

	// A single multi-row INSERT rather than a loop: MySQL has no batch
	// protocol, and one statement is one round trip where a loop is N.
	const row = `(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`

	placeholders := make([]string, len(records))
	args := make([]any, 0, len(records)*16)

	for i, r := range records {
		placeholders[i] = row

		occurredAt := r.OccurredAt
		if occurredAt.IsZero() {
			occurredAt = time.Now().UTC()
		}

		args = append(args,
			string(r.TenantID), occurredAt.UTC(),
			nullableID(r.ActorUserID), r.ActorLabel, r.Action,
			nullableID(r.RepositoryID), nullable(r.Branch), nullable(r.Path),
			nullable(string(r.Effect)), nullable(string(r.Reason)), nullable(r.RuleID), nullable(r.Guard),
			r.PolicyVersion, nullable(r.RequestID), nullableID(r.TaskID), nullJSON(r.Detail))
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO audit_log (
			tenant_id, occurred_at,
			actor_user_id, actor_label, action,
			repository_id, branch, path,
			effect, reason, rule_id, guard,
			policy_version, request_id, task_id, detail
		) VALUES `+strings.Join(placeholders, ", "), args...)

	return mapError(err)
}

func (s *auditStore) Query(ctx context.Context, q store.AuditQuery) ([]*store.AuditRecord, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	// Built rather than a chain of "(? IS NULL OR col = ?)": that form is
	// portable but opaque to the optimizer, which then scans where an index
	// would have served. The audit table is the one that grows without bound,
	// so it is the last place to hand the planner a predicate it cannot use.
	where := []string{"1 = 1"}
	args := []any{}

	if q.Tenant != "" {
		where = append(where, "tenant_id = ?")
		args = append(args, string(q.Tenant))
	}
	if q.ActorUserID != "" {
		where = append(where, "actor_user_id = ?")
		args = append(args, string(q.ActorUserID))
	}
	if q.RepositoryID != "" {
		where = append(where, "repository_id = ?")
		args = append(args, string(q.RepositoryID))
	}
	if q.RequestID != "" {
		where = append(where, "request_id = ?")
		args = append(args, q.RequestID)
	}
	if !q.Since.IsZero() {
		where = append(where, "occurred_at >= ?")
		args = append(args, q.Since.UTC())
	}
	if !q.Until.IsZero() {
		where = append(where, "occurred_at <= ?")
		args = append(args, q.Until.UTC())
	}

	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+auditColumns+`
		FROM audit_log
		WHERE `+strings.Join(where, " AND ")+`
		ORDER BY id DESC
		LIMIT ?`, args...)
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []*store.AuditRecord
	for rows.Next() {
		r, err := scanAudit(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}

	return out, mapError(rows.Err())
}

// nullJSON maps absent bytes to NULL.
//
// An empty slice is not the same as no value to a JSON column: MySQL rejects ""
// as invalid JSON, where PostgreSQL's jsonb would too. Callers that have
// nothing to record must produce NULL, not zero bytes.
func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// inPlaceholders renders "?, ?, ?" for n values, for an IN clause built at
// runtime.
func inPlaceholders(n int) string {
	if n <= 0 {
		return ""
	}

	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

var (
	_ store.SessionStore   = (*sessionStore)(nil)
	_ store.SyncPointStore = (*syncPointStore)(nil)
	_ store.ArtifactStore  = (*artifactStore)(nil)
	_ store.AuditStore     = (*auditStore)(nil)
)

// PruneAudit removes audit records older than a cutoff.
//
// The append-only trigger has to come off for the DELETE, and this backend has
// no way to disable one: MySQL and MariaDB only have DROP TRIGGER, which is
// DDL, which commits immediately and cannot be rolled back. So there is a real
// window during which the table accepts deletions.
//
// Three things make that acceptable rather than merely unavoidable:
//
// The window exists only in a session that already holds DDL rights. The
// application account has SELECT, INSERT, UPDATE and DELETE and nothing more —
// see docs/CONFIGURATION.md — so it cannot open this window, and an account
// that can could equally DROP TABLE. The guarantee has always been "an
// application bug cannot rewrite history", never "an operator cannot".
//
// Only the DELETE trigger is dropped. UPDATE stays refused throughout, so the
// window permits removal, never rewriting.
//
// The trigger is recreated with a context that ignores cancellation, so a
// Ctrl-C during a long purge still closes the window. What survives that is a
// hard kill, and the next prune reports it through GuardsWereMissing rather
// than leaving an operator to discover it from a schema dump.
func (s *Store) PruneAudit(ctx context.Context, before time.Time, batch int) (result store.PruneResult, retErr error) {
	if batch <= 0 {
		batch = 1000
	}

	// A pinned connection: DROP TRIGGER and the deletes must not be spread
	// across connections, and the recreation must land on one that is still
	// open.
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return result, mapError(err)
	}
	defer conn.Close()

	present, err := triggerExists(ctx, conn, "audit_log_no_delete")
	if err != nil {
		return result, err
	}

	result.GuardsWereMissing = !present

	if present {
		if _, err := conn.ExecContext(ctx, `DROP TRIGGER audit_log_no_delete`); err != nil {
			return result, fmt.Errorf("mysql: lift the append-only guard: %w", err)
		}
	}

	// Deferred rather than written after the loop: every path out of this
	// function, including a panic, has to close the window.
	defer func() {
		if _, err := conn.ExecContext(context.WithoutCancel(ctx), AuditNoDeleteTrigger); err != nil {
			// Joined into the returned error when there is one, and surfaced
			// on its own when there is not: an unprotected audit table is more
			// serious than a failed purge.
			err = fmt.Errorf("mysql: RESTORE THE APPEND-ONLY GUARD BY HAND — "+
				"audit_log accepts DELETE until trigger audit_log_no_delete is "+
				"recreated from migrations/mysql/0002: %w", err)

			if retErr == nil {
				retErr = err
			} else {
				retErr = errors.Join(retErr, err)
			}
		}
	}()

	for {
		deleted, err := conn.ExecContext(ctx,
			`DELETE FROM audit_log WHERE occurred_at < ? ORDER BY occurred_at LIMIT ?`,
			before.UTC(), batch)
		if err != nil {
			return result, mapError(err)
		}

		removed, err := deleted.RowsAffected()
		if err != nil {
			return result, mapError(err)
		}

		result.Removed += removed

		if removed < int64(batch) {
			return result, nil
		}

		// Between batches the context is checked, so a cancelled purge stops
		// having removed a whole number of batches rather than mid-statement.
		if err := ctx.Err(); err != nil {
			return result, err
		}
	}
}

// triggerExists reports whether a trigger of that name is defined on the
// current database.
func triggerExists(ctx context.Context, conn *sql.Conn, name string) (bool, error) {
	var count int

	err := conn.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.triggers
		WHERE trigger_schema = DATABASE() AND trigger_name = ?`, name).Scan(&count)
	if err != nil {
		return false, mapError(err)
	}

	return count > 0, nil
}

// auditNoDeleteTrigger recreates what PruneAudit drops.
//
// Kept byte-identical to migrations/mysql/0002_audit_append_only_trigger.up.sql,
// and a test compares the two: a prune that restored a weaker guard than it
// lifted would be worse than one that failed.
// AuditNoDeleteTrigger is exported so a test can compare it with the migration.
const AuditNoDeleteTrigger = `CREATE TRIGGER audit_log_no_delete BEFORE DELETE ON audit_log
    FOR EACH ROW SIGNAL SQLSTATE '45000'
    SET MESSAGE_TEXT = 'audit_log is append-only: DELETE is not permitted; to purge, DROP TRIGGER audit_log_no_delete, delete, then recreate it'`

var _ store.AuditPruner = (*Store)(nil)

// CountAuditBefore reports how many records a prune with this cutoff would
// remove.
func (s *Store) CountAuditBefore(ctx context.Context, before time.Time) (int64, error) {
	var count int64

	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM audit_log WHERE occurred_at < ?`, before.UTC()).Scan(&count)

	return count, mapError(err)
}
