package postgres

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NitScm/nit/internal/store/sqlmigrate"
)

const migrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version     INT PRIMARY KEY,
	name        TEXT NOT NULL,
	applied_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
)`

// Migrate applies every migration not yet recorded, in version order.
//
// Each migration runs in its own transaction together with the row that records
// it: a migration that fails halfway leaves neither partial schema nor a false
// record of having succeeded.
//
// A session-level advisory lock serializes concurrent starts, so rolling out
// several replicas at once does not race them all through the same DDL.
func Migrate(ctx context.Context, pool *pgxpool.Pool, migrations []sqlmigrate.Migration) (applied int, err error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return 0, fmt.Errorf("postgres: acquire: %w", err)
	}
	defer conn.Release()

	// An arbitrary but fixed key: every nit process migrating this database
	// must agree on it.
	const migrationLockKey = 0x6E697401 // "nit\x01"

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return 0, fmt.Errorf("postgres: migration lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, migrationLockKey)

	if _, err := conn.Exec(ctx, migrationsTable); err != nil {
		return 0, fmt.Errorf("postgres: create migrations table: %w", err)
	}

	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return 0, mapError(err)
	}

	done := make(map[int]bool)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return 0, err
		}
		done[v] = true
	}
	rows.Close()

	if err := rows.Err(); err != nil {
		return 0, err
	}

	for _, m := range migrations {
		if done[m.Version] {
			continue
		}

		tx, err := conn.Begin(ctx)
		if err != nil {
			return applied, mapError(err)
		}

		if _, err := tx.Exec(ctx, m.Up); err != nil {
			tx.Rollback(ctx)
			return applied, fmt.Errorf("postgres: migration %04d_%s: %w", m.Version, m.Name, err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES ($1, $2)`,
			m.Version, m.Name); err != nil {
			tx.Rollback(ctx)
			return applied, mapError(err)
		}

		if err := tx.Commit(ctx); err != nil {
			return applied, mapError(err)
		}

		applied++
	}

	return applied, nil
}
