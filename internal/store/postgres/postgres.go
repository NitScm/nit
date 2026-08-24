// Package postgres implements the store interfaces on PostgreSQL.
//
// It is the production backend. Correctness of the queue semantics is not
// asserted here but in pkg/store/storetest, the conformance suite this
// implementation shares with the in-memory one: the two must be
// indistinguishable to a caller, and only a shared suite can guarantee that.
//
// Two conventions run through the SQL. UUID columns are read as text and
// written with an explicit ::uuid cast, so the Go layer can keep store.ID as an
// opaque string rather than binding itself to a UUID type. And every dispatch
// query names its predicates in the same order as the partial indexes in
// migrations/0001_init.up.sql, so it stays obvious which index serves which
// query.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// Store is a PostgreSQL-backed store.Store.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects to PostgreSQL and verifies the connection.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("postgres: parse DSN: %w", err)
	}

	cfg.PrepareConn = prepareTenant

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("postgres: connect: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("postgres: ping: %w", err)
	}

	return &Store{pool: pool}, nil
}

// NewWithPool wraps an existing pool, for callers that manage it themselves.
func NewWithPool(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Pool exposes the underlying pool, for migrations and diagnostics.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

func (s *Store) Users() store.UserStore              { return &userStore{s.pool} }
func (s *Store) Sessions() store.SessionStore        { return &sessionStore{s.pool} }
func (s *Store) Workspaces() store.WorkspaceStore    { return &workspaceStore{s.pool} }
func (s *Store) Repositories() store.RepositoryStore { return &repositoryStore{s.pool} }
func (s *Store) SyncPoints() store.SyncPointStore    { return &syncPointStore{s.pool} }
func (s *Store) Tasks() store.TaskStore              { return &taskStore{s.pool} }
func (s *Store) Artifacts() store.ArtifactStore      { return &artifactStore{s.pool} }
func (s *Store) Audit() store.AuditStore             { return &auditStore{s.pool} }
func (s *Store) Tenants() store.TenantStore          { return &tenantStore{s.pool} }

// Close releases the pool.
func (s *Store) Close() error {
	s.pool.Close()
	return nil
}

var _ store.Store = (*Store)(nil)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// tenantSetting is what the row-level security policies read.
const tenantSetting = "nit.tenant"

// prepareTenant stamps the connection with the tenant of the context about to
// use it.
//
// This is the whole mechanism, and it is one hook rather than a change to forty
// query methods. pgx calls it as a connection leaves the pool, with the caller's
// context, so the setting always describes the request that is about to run —
// and a connection handed to a different tenant next is re-stamped before it is
// used.
//
// **A context with no tenant sets the empty string, deliberately.** The policies
// compare against it, nothing equals it, and a caller that forgot reads nothing
// instead of reading somebody else's rows. Failing closed is the only useful
// direction here: an empty result is a bug that gets reported, and a
// cross-tenant read is a bug that does not.
//
// Session-level rather than transaction-local, because most operations here are
// single statements with no transaction to be local to. That is safe only
// because every acquisition re-stamps: a stale value can never be *read*,
// because it is overwritten before the connection is used.
func prepareTenant(ctx context.Context, conn *pgx.Conn) (bool, error) {
	tenant := store.TenantFrom(ctx)

	if _, err := conn.Exec(ctx, "SELECT set_config($1, $2, false)",
		tenantSetting, string(tenant)); err != nil {
		// The connection is unusable for this purpose, so it is destroyed
		// rather than handed out with whatever the last request left on it.
		return false, err
	}

	return true, nil
}

// RowSecurityEnforced reports whether row-level security actually applies to
// this connection.
//
// It exists because the failure mode of RLS is silence. A superuser bypasses it
// entirely and a table's owner bypasses it unless the table is FORCEd, so a
// deployment can enable every policy, see nothing break, and be protected by
// none of them. Asking the database is the only way to know.
func (s *Store) RowSecurityEnforced(ctx context.Context) (bool, error) {
	var (
		superuser bool
		bypass    bool
	)

	err := s.pool.QueryRow(ctx, `
		SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user`).
		Scan(&superuser, &bypass)
	if err != nil {
		return false, mapError(err)
	}

	if superuser || bypass {
		return false, nil
	}

	// Owning a table bypasses its policies unless the table is forced, so both
	// have to be true for the answer to be yes.
	var enforced bool

	err = s.pool.QueryRow(ctx, `
		SELECT bool_and(relrowsecurity AND relforcerowsecurity)
		FROM pg_class
		WHERE relname = ANY($1) AND relkind = 'r'`, tenantScopedTables).Scan(&enforced)
	if err != nil {
		return false, mapError(err)
	}

	return enforced, nil
}

// tenantScopedTables are the tables a tenant's rows live in.
//
// sessions is deliberately absent. It is the table that *resolves* the tenant —
// authentication looks a token up before anyone knows whose it is — so it
// cannot be protected by the tenant without making authentication impossible.
// What protects it is that a token hash is 32 unguessable bytes with a unique
// constraint, which is the same thing that protects it today.
var tenantScopedTables = []string{
	"users", "workspaces", "repositories", "sync_points",
	"tasks", "artifacts", "audit_log", "partition_leases",
}

// mapError translates driver errors into the store vocabulary. Callers branch
// on store's sentinels; a pgx error leaking out would couple them to the
// backend.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return store.ErrNotFound
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		// Joined rather than replaced: callers match on store.ErrConflict, but
		// Create still has to tell a duplicate request id from any other unique
		// violation, and that needs the constraint name from the driver error.
		return errors.Join(store.ErrConflict, err)
	}

	return err
}

// isUniqueViolation reports whether err is a duplicate-key error on the named
// constraint.
func isUniqueViolation(err error, constraint string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}

// nullable maps the empty string to SQL NULL. Optional foreign keys are the
// empty ID in Go and NULL in the database; conflating the two would make an
// unset workspace point at nothing in a way the schema cannot express.
func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullableID(id store.ID) any { return nullable(string(id)) }

func derefString(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func derefID(p *string) store.ID { return store.ID(derefString(p)) }

func derefTime(p *time.Time) time.Time {
	if p == nil {
		return time.Time{}
	}
	return *p
}

// ---------------------------------------------------------------------------
// users
// ---------------------------------------------------------------------------

type userStore struct{ pool *pgxpool.Pool }

const userColumns = `id::text, tenant_id, policy_user_id, email, display_name, disabled, created_at, updated_at`

func scanUser(row pgx.Row) (*store.User, error) {
	var u store.User

	err := row.Scan(&u.ID, &u.TenantID, &u.PolicyUserID, &u.Email, &u.DisplayName,
		&u.Disabled, &u.CreatedAt, &u.UpdatedAt)
	if err != nil {
		return nil, mapError(err)
	}

	return &u, nil
}

func (s *userStore) ByID(ctx context.Context, id store.ID) (*store.User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = $1::uuid`, string(id)))
}

func (s *userStore) ByPolicyID(ctx context.Context, tenant policy.TenantID, policyID policy.UserID) (*store.User, error) {
	return scanUser(s.pool.QueryRow(ctx,
		`SELECT `+userColumns+` FROM users WHERE tenant_id = $1 AND policy_user_id = $2`,
		string(tenant), string(policyID)))
}

func (s *userStore) Upsert(ctx context.Context, u *store.User) (*store.User, error) {
	return scanUser(s.pool.QueryRow(ctx, `
		INSERT INTO users (tenant_id, policy_user_id, email, display_name, disabled)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (tenant_id, policy_user_id) DO UPDATE
		SET email = EXCLUDED.email,
		    display_name = EXCLUDED.display_name,
		    disabled = EXCLUDED.disabled,
		    updated_at = NOW()
		RETURNING `+userColumns,
		string(u.TenantID), string(u.PolicyUserID), u.Email, u.DisplayName, u.Disabled))
}

// ---------------------------------------------------------------------------
// workspaces
// ---------------------------------------------------------------------------

type workspaceStore struct{ pool *pgxpool.Pool }

const workspaceColumns = `id::text, tenant_id, user_id::text, label, created_at, last_seen_at`

func scanWorkspace(row pgx.Row) (*store.Workspace, error) {
	var w store.Workspace

	err := row.Scan(&w.ID, &w.TenantID, &w.UserID, &w.Label, &w.CreatedAt, &w.LastSeenAt)
	if err != nil {
		return nil, mapError(err)
	}

	return &w, nil
}

func (s *workspaceStore) ByID(ctx context.Context, id store.ID) (*store.Workspace, error) {
	return scanWorkspace(s.pool.QueryRow(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces WHERE id = $1::uuid`, string(id)))
}

func (s *workspaceStore) ListByUser(ctx context.Context, userID store.ID) ([]*store.Workspace, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces WHERE user_id = $1::uuid ORDER BY created_at, id`,
		string(userID))
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []*store.Workspace
	for rows.Next() {
		w, err := scanWorkspace(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}

	return out, mapError(rows.Err())
}

func (s *workspaceStore) Create(ctx context.Context, w *store.Workspace) (*store.Workspace, error) {
	return scanWorkspace(s.pool.QueryRow(ctx, `
		INSERT INTO workspaces (tenant_id, user_id, label)
		VALUES ($1, $2::uuid, $3)
		RETURNING `+workspaceColumns,
		string(w.TenantID), string(w.UserID), w.Label))
}

func (s *workspaceStore) Touch(ctx context.Context, id store.ID, at time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE workspaces SET last_seen_at = $2 WHERE id = $1::uuid`, string(id), at)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}

	return nil
}

// ---------------------------------------------------------------------------
// repositories
// ---------------------------------------------------------------------------

type repositoryStore struct{ pool *pgxpool.Pool }

const repositoryColumns = `id::text, tenant_id, policy_repo_id, remote, forge, default_branch, policy_version, created_at, updated_at`

func scanRepository(row pgx.Row) (*store.Repository, error) {
	var r store.Repository

	err := row.Scan(&r.ID, &r.TenantID, &r.PolicyRepoID, &r.Remote, &r.Forge,
		&r.DefaultBranch, &r.PolicyVersion, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, mapError(err)
	}

	return &r, nil
}

func (s *repositoryStore) ByID(ctx context.Context, id store.ID) (*store.Repository, error) {
	return scanRepository(s.pool.QueryRow(ctx,
		`SELECT `+repositoryColumns+` FROM repositories WHERE id = $1::uuid`, string(id)))
}

func (s *repositoryStore) ByPolicyID(ctx context.Context, tenant policy.TenantID, policyID policy.RepoID) (*store.Repository, error) {
	return scanRepository(s.pool.QueryRow(ctx,
		`SELECT `+repositoryColumns+` FROM repositories WHERE tenant_id = $1 AND policy_repo_id = $2`,
		string(tenant), string(policyID)))
}

func (s *repositoryStore) List(ctx context.Context, tenant policy.TenantID) ([]*store.Repository, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+repositoryColumns+` FROM repositories WHERE tenant_id = $1 ORDER BY policy_repo_id`,
		string(tenant))
	if err != nil {
		return nil, mapError(err)
	}
	defer rows.Close()

	var out []*store.Repository
	for rows.Next() {
		r, err := scanRepository(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}

	return out, mapError(rows.Err())
}

// Reconcile upserts every repository of the bundle in one transaction.
//
// Nothing is deleted: a repository removed from the bundle still has tasks and
// audit records pointing at it, and losing those to a configuration edit would
// be worse than keeping a stale row.
func (s *repositoryStore) Reconcile(ctx context.Context, tenant policy.TenantID, repos []*store.Repository) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mapError(err)
	}
	defer tx.Rollback(ctx)

	for _, r := range repos {
		_, err := tx.Exec(ctx, `
			INSERT INTO repositories (tenant_id, policy_repo_id, remote, forge, default_branch, policy_version)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (tenant_id, policy_repo_id) DO UPDATE
			SET remote = EXCLUDED.remote,
			    forge = EXCLUDED.forge,
			    default_branch = EXCLUDED.default_branch,
			    policy_version = EXCLUDED.policy_version,
			    updated_at = NOW()`,
			string(tenant), string(r.PolicyRepoID), r.Remote, r.Forge, r.DefaultBranch, r.PolicyVersion)
		if err != nil {
			return mapError(err)
		}
	}

	return mapError(tx.Commit(ctx))
}
