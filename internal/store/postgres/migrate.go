package postgres

import (
	"context"
	"fmt"
	"io/fs"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Migration is one schema version.
type Migration struct {
	Version int
	Name    string
	Up      string
	Down    string
}

// LoadMigrations reads migrations from a file system, expecting names of the
// form "0001_init.up.sql" and "0001_init.down.sql".
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("postgres: read migrations: %w", err)
	}

	byVersion := make(map[int]*Migration)

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}

		version, name, direction, err := parseMigrationName(e.Name())
		if err != nil {
			return nil, err
		}

		content, err := fs.ReadFile(fsys, e.Name())
		if err != nil {
			return nil, fmt.Errorf("postgres: read %s: %w", e.Name(), err)
		}

		m, ok := byVersion[version]
		if !ok {
			m = &Migration{Version: version, Name: name}
			byVersion[version] = m
		}

		if direction == "up" {
			m.Up = string(content)
		} else {
			m.Down = string(content)
		}
	}

	out := make([]Migration, 0, len(byVersion))
	for _, m := range byVersion {
		if m.Up == "" {
			return nil, fmt.Errorf("postgres: migration %d (%s) has no up file", m.Version, m.Name)
		}
		out = append(out, *m)
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })

	return out, nil
}

func parseMigrationName(filename string) (version int, name, direction string, err error) {
	base := strings.TrimSuffix(filename, ".sql")

	dot := strings.LastIndex(base, ".")
	if dot < 0 {
		return 0, "", "", fmt.Errorf("postgres: migration %q has no direction suffix", filename)
	}

	direction = base[dot+1:]
	if direction != "up" && direction != "down" {
		return 0, "", "", fmt.Errorf("postgres: migration %q has an unknown direction %q", filename, direction)
	}

	rest := base[:dot]

	underscore := strings.Index(rest, "_")
	if underscore < 0 {
		return 0, "", "", fmt.Errorf("postgres: migration %q has no version prefix", filename)
	}

	version, err = strconv.Atoi(rest[:underscore])
	if err != nil {
		return 0, "", "", fmt.Errorf("postgres: migration %q has a non-numeric version: %w", filename, err)
	}

	return version, rest[underscore+1:], direction, nil
}

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
func Migrate(ctx context.Context, pool *pgxpool.Pool, migrations []Migration) (applied int, err error) {
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
