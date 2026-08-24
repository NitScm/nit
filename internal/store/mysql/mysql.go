// Package mysql implements the store interfaces on MySQL and MariaDB.
//
// It exists because "which database do you support?" is an adoption question
// before it is a technical one: a team already running MariaDB will not stand
// up PostgreSQL to try a tool. PostgreSQL remains the recommended backend, and
// docs/CONFIGURATION.md says why.
//
// Correctness of the queue semantics is not asserted here but in
// pkg/store/storetest, the conformance suite this implementation shares with
// PostgreSQL and the in-memory store. All three must be indistinguishable to a
// caller, and only a shared suite can guarantee that.
//
// Three conventions run through the SQL, and each is a difference from the
// PostgreSQL implementation rather than a preference:
//
// Identifiers are generated in Go. Neither engine has UPDATE ... RETURNING and
// only MariaDB has INSERT ... RETURNING, so a caller has to know the id it
// inserted regardless; generating it here removes a round trip and a dialect
// difference at once.
//
// Every write that must return the stored row does INSERT then SELECT. Inside
// a transaction, or keyed by a value the caller supplied, that reads back what
// it just wrote.
//
// Times crossing this boundary are UTC. The DSN is required to carry
// parseTime=true and loc=UTC, and Open refuses one that does not — a store that
// silently shifted every timestamp by the server's zone would corrupt lease
// expiry and the audit trail at once.
package mysql

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/go-sql-driver/mysql"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// Store is a MySQL- or MariaDB-backed store.Store.
type Store struct {
	db *sql.DB
}

// Open connects and verifies the connection.
//
// The DSN is the go-sql-driver form, "user:pass@tcp(host:3306)/database".
func Open(ctx context.Context, dsn string) (*Store, error) {
	prepared, err := prepareDSN(dsn)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("mysql", prepared)
	if err != nil {
		return nil, fmt.Errorf("mysql: connect: %w", err)
	}

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("mysql: ping: %w", err)
	}

	return &Store{db: db}, nil
}

// NewWithDB wraps an existing handle, for callers that manage it themselves.
//
// Nothing checks the DSN behind it. A handle whose connections do not parse
// time in UTC will misread every timestamp in the schema.
func NewWithDB(db *sql.DB) *Store { return &Store{db: db} }

// DB exposes the underlying handle, for migrations and diagnostics.
func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) Users() store.UserStore              { return &userStore{s.db} }
func (s *Store) Sessions() store.SessionStore        { return &sessionStore{s.db} }
func (s *Store) Workspaces() store.WorkspaceStore    { return &workspaceStore{s.db} }
func (s *Store) Repositories() store.RepositoryStore { return &repositoryStore{s.db} }
func (s *Store) SyncPoints() store.SyncPointStore    { return &syncPointStore{s.db} }
func (s *Store) Tasks() store.TaskStore              { return &taskStore{s.db} }
func (s *Store) Artifacts() store.ArtifactStore      { return &artifactStore{s.db} }
func (s *Store) Audit() store.AuditStore             { return &auditStore{s.db} }
func (s *Store) Tenants() store.TenantStore          { return &tenantStore{s.db} }

// Close releases the handle.
func (s *Store) Close() error { return s.db.Close() }

var _ store.Store = (*Store)(nil)

// ---------------------------------------------------------------------------
// DSN
// ---------------------------------------------------------------------------

// prepareDSN applies the settings the schema depends on, and refuses a DSN that
// asks for different ones.
//
// It does not silently override. An operator who wrote loc=Local meant
// something by it, and quietly ignoring that would produce a deployment whose
// timestamps are wrong in a way nothing reports.
func prepareDSN(dsn string) (string, error) {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		// A common shape mistake, worth naming: the URL form works for
		// PostgreSQL and is not what this driver reads.
		if strings.HasPrefix(dsn, "mysql://") || strings.HasPrefix(dsn, "mariadb://") {
			return "", fmt.Errorf("mysql: the DSN is a URL; this driver wants "+
				"user:password@tcp(host:3306)/database (%w)", err)
		}
		return "", fmt.Errorf("mysql: parse DSN: %w", err)
	}

	if cfg.Loc != nil && cfg.Loc != time.UTC {
		return "", fmt.Errorf("mysql: the DSN sets loc=%s; nit stores UTC and needs loc=UTC", cfg.Loc)
	}

	cfg.Loc = time.UTC
	cfg.ParseTime = true
	cfg.ClientFoundRows = true

	// RowsAffected must count rows *matched*, not rows changed.
	//
	// MySQL's default reports zero for an UPDATE that assigns a column the
	// value it already held, and this package reads that count to decide
	// whether a row existed: a heartbeat extending a lease to the instant it
	// already carried would be mistaken for a lost lease, and a session revoked
	// twice for a session that never existed. PostgreSQL writes the row either
	// way, so this is what makes the two agree.

	return cfg.FormatDSN(), nil
}

// RedactDSN renders a DSN without its password, for logs and `config show`.
func RedactDSN(dsn string) string {
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		return "(unparseable dsn)"
	}

	if cfg.Passwd != "" {
		cfg.Passwd = "***"
	}

	return cfg.FormatDSN()
}

// IsDSN reports whether a database URL names this backend.
//
// Both forms are recognised. The URL form is not valid for the driver — Open
// rejects it with an explanation — but recognising it is what lets that
// explanation be printed instead of "unsupported database".
func IsDSN(raw string) bool {
	if strings.HasPrefix(raw, "mysql://") || strings.HasPrefix(raw, "mariadb://") {
		return true
	}

	// Anything else carrying a scheme belongs to another driver. Testing for
	// "://" rather than asking net/url: it parses "root:nit@tcp(host)/db" as a
	// URL with the scheme "root", so a scheme check would reject the very DSNs
	// this function exists to recognise.
	if strings.Contains(raw, "://") {
		return false
	}

	_, err := mysql.ParseDSN(raw)

	return err == nil
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// scanner is what *sql.Row and *sql.Rows have in common.
type scanner interface {
	Scan(dest ...any) error
}

// querier is what *sql.DB and *sql.Tx have in common, so a statement can be
// written once and run inside a transaction or outside one.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// newID returns a random UUID v4 in the textual form the schema stores.
//
// A dependency-free generator rather than a UUID package: this is the only
// place nit needs one, and rand.Read is the whole implementation.
func newID() store.ID {
	var b [16]byte

	// crypto/rand.Read cannot fail on any platform Go supports; it panics
	// internally rather than returning an error.
	rand.Read(b[:])

	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10

	const hex = "0123456789abcdef"

	out := make([]byte, 36)
	i := 0

	for n, v := range b {
		if n == 4 || n == 6 || n == 8 || n == 10 {
			out[i] = '-'
			i++
		}

		out[i] = hex[v>>4]
		out[i+1] = hex[v&0x0f]
		i += 2
	}

	return store.ID(out)
}

// mapError translates driver errors into the store vocabulary. Callers branch
// on store's sentinels; a driver error leaking out would couple them to the
// backend.
func mapError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return store.ErrNotFound
	}

	var mysqlErr *mysql.MySQLError
	if errors.As(err, &mysqlErr) {
		switch mysqlErr.Number {
		case 1062: // ER_DUP_ENTRY
			// Joined rather than replaced: callers match on store.ErrConflict,
			// but Create still has to tell a duplicate request id from any
			// other unique violation, and that needs the key name from the
			// driver error.
			return errors.Join(store.ErrConflict, err)
		case 1213, 1205: // deadlock, lock wait timeout
			// Both mean "retry", not "your data was rejected". Callers that
			// distinguish them get a conflict, which is the retryable class.
			return errors.Join(store.ErrConflict, err)
		}
	}

	return err
}

// isDuplicate reports whether err is a duplicate-key error on the named
// constraint.
//
// MySQL does not expose the constraint name as a field the way PostgreSQL does;
// it is inside the message, as "for key 'tasks.tasks_request_id_unique'". A
// substring match is what is available.
func isDuplicate(err error, constraint string) bool {
	var mysqlErr *mysql.MySQLError

	return errors.As(err, &mysqlErr) &&
		mysqlErr.Number == 1062 &&
		strings.Contains(mysqlErr.Message, constraint)
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

// nullTime maps an absent or zero instant to NULL.
func nullTime(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC()
}

// nullInstant is nullTime for a value rather than a pointer, where the zero
// instant is what "unset" looks like.
func nullInstant(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

func text(v sql.NullString) string {
	if !v.Valid {
		return ""
	}
	return v.String
}

func textID(v sql.NullString) store.ID { return store.ID(text(v)) }

// timePtr converts a nullable column into the optional field it maps to.
func timePtr(v sql.NullTime) *time.Time {
	if !v.Valid {
		return nil
	}

	at := v.Time.UTC()

	return &at
}

func deref(t sql.NullTime) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}

// ---------------------------------------------------------------------------
// users
// ---------------------------------------------------------------------------

type userStore struct{ db *sql.DB }

const userColumns = `id, tenant_id, policy_user_id, email, display_name, disabled, created_at, updated_at`

func scanUser(row scanner) (*store.User, error) {
	var (
		u         store.User
		created   sql.NullTime
		updated   sql.NullTime
		tenantID  string
		policyID  string
		userID    string
		disabled  bool
		email     string
		displayed string
	)

	err := row.Scan(&userID, &tenantID, &policyID, &email, &displayed,
		&disabled, &created, &updated)
	if err != nil {
		return nil, mapError(err)
	}

	u.ID = store.ID(userID)
	u.TenantID = policy.TenantID(tenantID)
	u.PolicyUserID = policy.UserID(policyID)
	u.Email = email
	u.DisplayName = displayed
	u.Disabled = disabled
	u.CreatedAt = deref(created)
	u.UpdatedAt = deref(updated)

	return &u, nil
}

func (s *userStore) ByID(ctx context.Context, id store.ID) (*store.User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE id = ?`, string(id)))
}

func (s *userStore) ByPolicyID(ctx context.Context, tenant policy.TenantID, policyID policy.UserID) (*store.User, error) {
	return scanUser(s.db.QueryRowContext(ctx,
		`SELECT `+userColumns+` FROM users WHERE tenant_id = ? AND policy_user_id = ?`,
		string(tenant), string(policyID)))
}

// Upsert writes the row and reads it back by its natural key.
//
// The read is by (tenant, policy_user_id) rather than by the id generated here,
// because on the update path the stored row keeps the id it already had.
func (s *userStore) Upsert(ctx context.Context, u *store.User) (*store.User, error) {
	// The update clause repeats the parameters instead of using VALUES(), which
	// MySQL deprecated in 8.0.20 and whose replacement — a row alias — MariaDB
	// does not accept. Repetition is the only spelling both engines agree on.
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (id, tenant_id, policy_user_id, email, display_name, disabled)
		VALUES (?, ?, ?, ?, ?, ?)
		ON DUPLICATE KEY UPDATE
			email = ?,
			display_name = ?,
			disabled = ?,
			updated_at = CURRENT_TIMESTAMP(6)`,
		string(newID()), string(u.TenantID), string(u.PolicyUserID),
		u.Email, u.DisplayName, u.Disabled,
		u.Email, u.DisplayName, u.Disabled)
	if err != nil {
		return nil, mapError(err)
	}

	return s.ByPolicyID(ctx, u.TenantID, u.PolicyUserID)
}

// ---------------------------------------------------------------------------
// workspaces
// ---------------------------------------------------------------------------

type workspaceStore struct{ db *sql.DB }

const workspaceColumns = `id, tenant_id, user_id, label, created_at, last_seen_at`

func scanWorkspace(row scanner) (*store.Workspace, error) {
	var (
		w        store.Workspace
		id       string
		tenantID string
		userID   string
		label    string
		created  sql.NullTime
		lastSeen sql.NullTime
	)

	if err := row.Scan(&id, &tenantID, &userID, &label, &created, &lastSeen); err != nil {
		return nil, mapError(err)
	}

	w.ID = store.ID(id)
	w.TenantID = policy.TenantID(tenantID)
	w.UserID = store.ID(userID)
	w.Label = label
	w.CreatedAt = deref(created)
	w.LastSeenAt = timePtr(lastSeen)

	return &w, nil
}

func (s *workspaceStore) ByID(ctx context.Context, id store.ID) (*store.Workspace, error) {
	return scanWorkspace(s.db.QueryRowContext(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces WHERE id = ?`, string(id)))
}

func (s *workspaceStore) ListByUser(ctx context.Context, userID store.ID) ([]*store.Workspace, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+workspaceColumns+` FROM workspaces WHERE user_id = ? ORDER BY created_at, id`,
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
	id := newID()

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO workspaces (id, tenant_id, user_id, label)
		VALUES (?, ?, ?, ?)`,
		string(id), string(w.TenantID), string(w.UserID), w.Label)
	if err != nil {
		return nil, mapError(err)
	}

	return s.ByID(ctx, id)
}

func (s *workspaceStore) Touch(ctx context.Context, id store.ID, at time.Time) error {
	result, err := s.db.ExecContext(ctx,
		`UPDATE workspaces SET last_seen_at = ? WHERE id = ?`, at.UTC(), string(id))
	if err != nil {
		return mapError(err)
	}

	return requireRow(result)
}

// requireRow turns "the UPDATE matched nothing" into ErrNotFound.
//
// It reads matched rows rather than changed ones, which holds only because
// prepareDSN sets clientFoundRows. Without that, an update writing a value a
// row already carried would be reported as a row that does not exist.
func requireRow(result sql.Result) error {
	affected, err := result.RowsAffected()
	if err != nil {
		return mapError(err)
	}
	if affected == 0 {
		return store.ErrNotFound
	}

	return nil
}

// ---------------------------------------------------------------------------
// repositories
// ---------------------------------------------------------------------------

type repositoryStore struct{ db *sql.DB }

const repositoryColumns = `id, tenant_id, policy_repo_id, remote, forge, default_branch, policy_version, created_at, updated_at`

func scanRepository(row scanner) (*store.Repository, error) {
	var (
		r        store.Repository
		id       string
		tenantID string
		policyID string
		created  sql.NullTime
		updated  sql.NullTime
	)

	err := row.Scan(&id, &tenantID, &policyID, &r.Remote, &r.Forge,
		&r.DefaultBranch, &r.PolicyVersion, &created, &updated)
	if err != nil {
		return nil, mapError(err)
	}

	r.ID = store.ID(id)
	r.TenantID = policy.TenantID(tenantID)
	r.PolicyRepoID = policy.RepoID(policyID)
	r.CreatedAt = deref(created)
	r.UpdatedAt = deref(updated)

	return &r, nil
}

func (s *repositoryStore) ByID(ctx context.Context, id store.ID) (*store.Repository, error) {
	return scanRepository(s.db.QueryRowContext(ctx,
		`SELECT `+repositoryColumns+` FROM repositories WHERE id = ?`, string(id)))
}

func (s *repositoryStore) ByPolicyID(ctx context.Context, tenant policy.TenantID, policyID policy.RepoID) (*store.Repository, error) {
	return scanRepository(s.db.QueryRowContext(ctx,
		`SELECT `+repositoryColumns+` FROM repositories WHERE tenant_id = ? AND policy_repo_id = ?`,
		string(tenant), string(policyID)))
}

func (s *repositoryStore) List(ctx context.Context, tenant policy.TenantID) ([]*store.Repository, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+repositoryColumns+` FROM repositories WHERE tenant_id = ? ORDER BY policy_repo_id`,
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
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return mapError(err)
	}
	defer tx.Rollback()

	for _, r := range repos {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO repositories (id, tenant_id, policy_repo_id, remote, forge, default_branch, policy_version)
			VALUES (?, ?, ?, ?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE
				remote = ?,
				forge = ?,
				default_branch = ?,
				policy_version = ?,
				updated_at = CURRENT_TIMESTAMP(6)`,
			string(newID()), string(tenant), string(r.PolicyRepoID),
			r.Remote, r.Forge, r.DefaultBranch, r.PolicyVersion,
			r.Remote, r.Forge, r.DefaultBranch, r.PolicyVersion)
		if err != nil {
			return mapError(err)
		}
	}

	return mapError(tx.Commit())
}
