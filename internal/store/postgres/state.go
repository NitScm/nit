package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NitScm/nit/internal/store"
	"github.com/NitScm/nit/pkg/policy"
)

// ---------------------------------------------------------------------------
// sync points
// ---------------------------------------------------------------------------

type syncPointStore struct{ pool *pgxpool.Pool }

const syncPointColumns = `tenant_id, workspace_id::text, repository_id::text, branch, upstream_commit, policy_version, created_at, updated_at`

func scanSyncPoint(row pgx.Row) (*store.SyncPoint, error) {
	var sp store.SyncPoint

	err := row.Scan(&sp.TenantID, &sp.WorkspaceID, &sp.RepositoryID, &sp.Branch,
		&sp.UpstreamCommit, &sp.PolicyVersion, &sp.CreatedAt, &sp.UpdatedAt)
	if err != nil {
		return nil, mapError(err)
	}

	return &sp, nil
}

func (s *syncPointStore) Get(ctx context.Context, workspaceID, repositoryID store.ID, branch string) (*store.SyncPoint, error) {
	return scanSyncPoint(s.pool.QueryRow(ctx, `
		SELECT `+syncPointColumns+`
		FROM sync_points
		WHERE workspace_id = $1::uuid AND repository_id = $2::uuid AND branch = $3`,
		string(workspaceID), string(repositoryID), branch))
}

func (s *syncPointStore) Put(ctx context.Context, sp *store.SyncPoint) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sync_points (tenant_id, workspace_id, repository_id, branch, upstream_commit, policy_version)
		VALUES ($1, $2::uuid, $3::uuid, $4, $5, $6)
		ON CONFLICT (workspace_id, repository_id, branch) DO UPDATE
		SET upstream_commit = EXCLUDED.upstream_commit,
		    policy_version = EXCLUDED.policy_version,
		    updated_at = NOW()`,
		string(sp.TenantID), string(sp.WorkspaceID), string(sp.RepositoryID),
		sp.Branch, sp.UpstreamCommit, sp.PolicyVersion)

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
		// Creating a sync point where none existed. ON CONFLICT DO NOTHING
		// makes the insert lose the race rather than overwrite a point another
		// operation established in the meantime.
		tag, err := s.pool.Exec(ctx, `
			INSERT INTO sync_points (tenant_id, workspace_id, repository_id, branch, upstream_commit, policy_version)
			VALUES ($1, $2::uuid, $3::uuid, $4, $5, $6)
			ON CONFLICT (workspace_id, repository_id, branch) DO NOTHING`,
			string(sp.TenantID), string(sp.WorkspaceID), string(sp.RepositoryID),
			sp.Branch, sp.UpstreamCommit, sp.PolicyVersion)
		if err != nil {
			return mapError(err)
		}
		if tag.RowsAffected() == 0 {
			return store.ErrConflict
		}
		return nil
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE sync_points
		SET upstream_commit = $5, policy_version = $6, updated_at = NOW()
		WHERE workspace_id = $1::uuid AND repository_id = $2::uuid AND branch = $3
		  AND upstream_commit = $4`,
		string(sp.WorkspaceID), string(sp.RepositoryID), sp.Branch,
		expectedCommit, sp.UpstreamCommit, sp.PolicyVersion)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 1 {
		return nil
	}

	// Distinguish "never existed" from "moved under us": the first means the
	// client must clone, the second that it must pull.
	var exists bool
	err = s.pool.QueryRow(ctx, `
		SELECT true FROM sync_points
		WHERE workspace_id = $1::uuid AND repository_id = $2::uuid AND branch = $3`,
		string(sp.WorkspaceID), string(sp.RepositoryID), sp.Branch).Scan(&exists)
	if err != nil {
		return mapError(err)
	}

	return store.ErrConflict
}

func (s *syncPointStore) ListByWorkspace(ctx context.Context, workspaceID store.ID) ([]*store.SyncPoint, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT `+syncPointColumns+`
		FROM sync_points
		WHERE workspace_id = $1::uuid
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
	_, err := s.pool.Exec(ctx, `
		DELETE FROM sync_points
		WHERE workspace_id = $1::uuid AND repository_id = $2::uuid AND branch = $3`,
		string(workspaceID), string(repositoryID), branch)

	return mapError(err)
}

// ---------------------------------------------------------------------------
// artifacts
// ---------------------------------------------------------------------------

type artifactStore struct{ pool *pgxpool.Pool }

const artifactColumns = `id::text, tenant_id, digest, kind, size_bytes, uncompressed_bytes, encoding, locator, created_at, expires_at`

func scanArtifact(row pgx.Row) (*store.Artifact, error) {
	var a store.Artifact

	err := row.Scan(&a.ID, &a.TenantID, &a.Digest, &a.Kind, &a.Size,
		&a.UncompressedSize, &a.Encoding, &a.Locator, &a.CreatedAt, &a.ExpiresAt)
	if err != nil {
		return nil, mapError(err)
	}

	return &a, nil
}

func (s *artifactStore) ByDigest(ctx context.Context, tenant policy.TenantID, digest string) (*store.Artifact, error) {
	return scanArtifact(s.pool.QueryRow(ctx,
		`SELECT `+artifactColumns+` FROM artifacts WHERE tenant_id = $1 AND digest = $2`,
		string(tenant), digest))
}

// Create records a blob, returning the existing record for identical bytes:
// content addressing means the same digest is the same artifact, and storing it
// twice would only create two rows pointing at one file.
func (s *artifactStore) Create(ctx context.Context, a *store.Artifact) (*store.Artifact, error) {
	created, err := scanArtifact(s.pool.QueryRow(ctx, `
		INSERT INTO artifacts (tenant_id, digest, kind, size_bytes, uncompressed_bytes, encoding, locator, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9::timestamptz, NOW()))
		ON CONFLICT (tenant_id, digest) DO UPDATE
		SET expires_at = COALESCE(EXCLUDED.expires_at, artifacts.expires_at)
		RETURNING `+artifactColumns,
		string(a.TenantID), a.Digest, string(a.Kind), a.Size,
		a.UncompressedSize, string(a.Encoding), a.Locator, a.ExpiresAt, nullableTime(a.CreatedAt)))
	if err != nil {
		return nil, err
	}

	return created, nil
}

func (s *artifactStore) Expired(ctx context.Context, now time.Time, limit int) ([]*store.Artifact, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+artifactColumns+`
		FROM artifacts
		WHERE expires_at IS NOT NULL AND expires_at <= $1
		ORDER BY expires_at
		LIMIT $2`, now, limit)
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
	_, err := s.pool.Exec(ctx, `DELETE FROM artifacts WHERE id = $1::uuid`, string(id))
	return mapError(err)
}

// ---------------------------------------------------------------------------
// audit
// ---------------------------------------------------------------------------

type auditStore struct{ pool *pgxpool.Pool }

const auditColumns = `
	id, tenant_id, occurred_at,
	actor_user_id::text, actor_label, action,
	repository_id::text, branch, path,
	effect, reason, rule_id, guard,
	policy_version, request_id, task_id::text, detail`

func scanAudit(row pgx.Row) (*store.AuditRecord, error) {
	var (
		r            store.AuditRecord
		actorUserID  *string
		repositoryID *string
		branch       *string
		path         *string
		effect       *string
		reason       *string
		ruleID       *string
		guard        *string
		requestID    *string
		taskID       *string
	)

	err := row.Scan(
		&r.ID, &r.TenantID, &r.OccurredAt,
		&actorUserID, &r.ActorLabel, &r.Action,
		&repositoryID, &branch, &path,
		&effect, &reason, &ruleID, &guard,
		&r.PolicyVersion, &requestID, &taskID, &r.Detail,
	)
	if err != nil {
		return nil, mapError(err)
	}

	r.ActorUserID = derefID(actorUserID)
	r.RepositoryID = derefID(repositoryID)
	r.Branch = derefString(branch)
	r.Path = derefString(path)
	r.Effect = policy.Effect(derefString(effect))
	r.Reason = policy.Reason(derefString(reason))
	r.RuleID = derefString(ruleID)
	r.Guard = derefString(guard)
	r.RequestID = derefString(requestID)
	r.TaskID = derefID(taskID)

	return &r, nil
}

// Append writes records in one batch. The table carries DO INSTEAD NOTHING
// rules against UPDATE and DELETE, so a bug in the application cannot rewrite
// history even if it tries.
func (s *auditStore) Append(ctx context.Context, records ...*store.AuditRecord) error {
	if len(records) == 0 {
		return nil
	}

	batch := &pgx.Batch{}

	for _, r := range records {
		occurredAt := r.OccurredAt
		if occurredAt.IsZero() {
			occurredAt = time.Now().UTC()
		}

		batch.Queue(`
			INSERT INTO audit_log (
				tenant_id, occurred_at,
				actor_user_id, actor_label, action,
				repository_id, branch, path,
				effect, reason, rule_id, guard,
				policy_version, request_id, task_id, detail
			) VALUES (
				$1, $2,
				$3::uuid, $4, $5,
				$6::uuid, $7, $8,
				$9::audit_effect, $10, $11, $12,
				$13, $14, $15::uuid, $16
			)`,
			string(r.TenantID), occurredAt,
			nullableID(r.ActorUserID), r.ActorLabel, r.Action,
			nullableID(r.RepositoryID), nullable(r.Branch), nullable(r.Path),
			nullable(string(r.Effect)), nullable(string(r.Reason)), nullable(r.RuleID), nullable(r.Guard),
			r.PolicyVersion, nullable(r.RequestID), nullableID(r.TaskID), r.Detail)
	}

	results := s.pool.SendBatch(ctx, batch)
	defer results.Close()

	for range records {
		if _, err := results.Exec(); err != nil {
			return mapError(err)
		}
	}

	return nil
}

func (s *auditStore) Query(ctx context.Context, q store.AuditQuery) ([]*store.AuditRecord, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = 100
	}

	rows, err := s.pool.Query(ctx, `
		SELECT `+auditColumns+`
		FROM audit_log
		WHERE ($1 = '' OR tenant_id = $1)
		  AND ($2::text IS NULL OR actor_user_id = $2::uuid)
		  AND ($3::text IS NULL OR repository_id = $3::uuid)
		  AND ($4 = '' OR request_id = $4)
		  AND ($5::timestamptz IS NULL OR occurred_at >= $5)
		  AND ($6::timestamptz IS NULL OR occurred_at <= $6)
		ORDER BY id DESC
		LIMIT $7`,
		string(q.Tenant),
		nullableID(q.ActorUserID),
		nullableID(q.RepositoryID),
		q.RequestID,
		nullableTime(q.Since),
		nullableTime(q.Until),
		limit)
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

func nullableTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

var (
	_ store.SyncPointStore = (*syncPointStore)(nil)
	_ store.ArtifactStore  = (*artifactStore)(nil)
	_ store.AuditStore     = (*auditStore)(nil)
)
