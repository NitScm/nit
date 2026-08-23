package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

type sessionStore struct{ pool *pgxpool.Pool }

const sessionColumns = `id::text, tenant_id, user_id::text, token_hash, label, created_at, expires_at, last_used_at, revoked_at`

func scanSession(row pgx.Row) (*store.Session, error) {
	var s store.Session

	err := row.Scan(&s.ID, &s.TenantID, &s.UserID, &s.TokenHash, &s.Label,
		&s.CreatedAt, &s.ExpiresAt, &s.LastUsedAt, &s.RevokedAt)
	if err != nil {
		return nil, mapError(err)
	}

	return &s, nil
}

func (s *sessionStore) Create(ctx context.Context, sess *store.Session) (*store.Session, error) {
	return scanSession(s.pool.QueryRow(ctx, `
		INSERT INTO sessions (tenant_id, user_id, token_hash, label, expires_at)
		VALUES ($1, $2::uuid, $3, $4, $5)
		RETURNING `+sessionColumns,
		string(sess.TenantID), string(sess.UserID), sess.TokenHash, sess.Label, sess.ExpiresAt))
}

// ByTokenHash finds a session by the hash of its token.
//
// Revoked and expired sessions are returned too. The caller decides what to do
// with them, so that an expired token can be reported as expired instead of
// looking like a token that never existed — the difference between "log in
// again" and "something is very wrong".
func (s *sessionStore) ByTokenHash(ctx context.Context, tenant policy.TenantID, hash []byte) (*store.Session, error) {
	return scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE tenant_id = $1 AND token_hash = $2`,
		string(tenant), hash))
}

func (s *sessionStore) ByID(ctx context.Context, id store.ID) (*store.Session, error) {
	return scanSession(s.pool.QueryRow(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = $1::uuid`, string(id)))
}

func (s *sessionStore) ListByUser(ctx context.Context, userID store.ID) ([]*store.Session, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE user_id = $1::uuid ORDER BY created_at, id`,
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
	tag, err := s.pool.Exec(ctx,
		`UPDATE sessions SET last_used_at = $2 WHERE id = $1::uuid`, string(id), at)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}

	return nil
}

// Revoke marks a session unusable. A second call keeps the first instant: for
// an incident timeline, when a credential was first cut off is the fact that
// matters.
func (s *sessionStore) Revoke(ctx context.Context, id store.ID, at time.Time) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE sessions SET revoked_at = COALESCE(revoked_at, $2) WHERE id = $1::uuid`,
		string(id), at)
	if err != nil {
		return mapError(err)
	}
	if tag.RowsAffected() == 0 {
		return store.ErrNotFound
	}

	return nil
}

var _ store.SessionStore = (*sessionStore)(nil)
