package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/NitScm/nit/internal/store/sqlmigrate"
)

const migrationsTable = `
CREATE TABLE IF NOT EXISTS schema_migrations (
	version     INT NOT NULL,
	name        VARCHAR(255) NOT NULL,
	applied_at  DATETIME(6) NOT NULL DEFAULT CURRENT_TIMESTAMP(6),

	PRIMARY KEY (version)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin`

// migrationLock serializes concurrent starts, so rolling out several replicas
// at once does not race them all through the same DDL. Scoped to the database
// for the same reason as the claim lock.
const migrationLock = `CONCAT(DATABASE(), ':nit:migrate')`

// Migrate applies every migration not yet recorded, in version order.
//
// One guarantee of the PostgreSQL implementation is absent here, and it cannot
// be recovered: **DDL is not transactional**. MySQL and MariaDB commit
// implicitly at each CREATE, ALTER or DROP, so a migration that fails halfway
// leaves the statements before the failure applied and no record that the
// version ran. The version is recorded only after the whole file succeeds, so
// a retry re-runs it from the top — which will fail on the tables that already
// exist.
//
// That is why each statement is executed separately rather than as one blob:
// the error names the statement that failed, which is the difference between
// an operator fixing the schema in a minute and restoring a backup. It is also
// why docs/CONFIGURATION.md tells an operator to back up before migrating this
// backend and not the other one.
func Migrate(ctx context.Context, db *sql.DB, migrations []sqlmigrate.Migration) (applied int, err error) {
	conn, err := db.Conn(ctx)
	if err != nil {
		return 0, fmt.Errorf("mysql: acquire: %w", err)
	}
	defer conn.Close()

	var got sql.NullInt64
	if err := conn.QueryRowContext(ctx,
		`SELECT GET_LOCK(`+migrationLock+`, ?)`, 30).Scan(&got); err != nil {
		return 0, fmt.Errorf("mysql: migration lock: %w", err)
	}
	if !got.Valid || got.Int64 != 1 {
		return 0, fmt.Errorf("mysql: migration lock: not acquired; another migration is running")
	}

	defer conn.ExecContext(context.WithoutCancel(ctx), `DO RELEASE_LOCK(`+migrationLock+`)`)

	if _, err := conn.ExecContext(ctx, migrationsTable); err != nil {
		return 0, fmt.Errorf("mysql: create migrations table: %w", err)
	}

	rows, err := conn.QueryContext(ctx, `SELECT version FROM schema_migrations`)
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

		statements, err := SplitStatements(m.Up)
		if err != nil {
			return applied, fmt.Errorf("mysql: migration %04d_%s: %w", m.Version, m.Name, err)
		}

		for i, statement := range statements {
			if _, err := conn.ExecContext(ctx, statement); err != nil {
				return applied, fmt.Errorf(
					"mysql: migration %04d_%s, statement %d of %d: %w\n"+
						"the statements before it are applied and the version is not recorded; "+
						"this backend cannot roll back DDL",
					m.Version, m.Name, i+1, len(statements), err)
			}
		}

		if _, err := conn.ExecContext(ctx,
			`INSERT INTO schema_migrations (version, name) VALUES (?, ?)`,
			m.Version, m.Name); err != nil {
			return applied, mapError(err)
		}

		applied++
	}

	return applied, nil
}

// SplitStatements breaks a migration file into the statements it contains.
//
// The MySQL protocol carries one statement per round trip unless a connection
// opts into multiStatements, which this package will not do: it is the flag
// that turns any injection into arbitrary statement execution, and enabling it
// on the migration path only would still leave it enabled on a connection the
// pool later reuses.
//
// So the splitting happens here. It tracks quoting, because a semicolon inside
// a string or an identifier ends nothing, and comments, because a semicolon
// inside one ends nothing either. It deliberately does not implement DELIMITER:
// that is a directive of the mysql(1) client which the server never receives,
// and migrations in this repository are written without compound statements for
// exactly that reason.
func SplitStatements(script string) ([]string, error) {
	var (
		out       []string
		current   strings.Builder
		quote     byte // 0, '\'', '"' or '`'
		escaped   bool
		lineComm  bool
		blockComm bool
	)

	flush := func() {
		statement := strings.TrimSpace(current.String())
		if statement != "" {
			out = append(out, statement)
		}
		current.Reset()
	}

	for i := 0; i < len(script); i++ {
		c := script[i]

		switch {
		case lineComm:
			if c == '\n' {
				lineComm = false
				current.WriteByte(c)
			}
			continue

		case blockComm:
			if c == '*' && i+1 < len(script) && script[i+1] == '/' {
				blockComm = false
				i++
			}
			continue

		case quote != 0:
			current.WriteByte(c)

			if escaped {
				escaped = false
				continue
			}

			switch {
			case c == '\\' && quote != '`':
				// Backquoted identifiers do not use backslash escapes; inside
				// them a backslash is a literal character.
				escaped = true
			case c == quote:
				quote = 0
			}

			continue
		}

		// Outside quotes and comments.
		switch {
		case c == '-' && i+1 < len(script) && script[i+1] == '-' &&
			(i+2 >= len(script) || script[i+2] == ' ' || script[i+2] == '\t' || script[i+2] == '\n'):
			// "--" starts a comment only when followed by whitespace, which is
			// what keeps it from swallowing an expression like "a--b".
			lineComm = true
			i++

		case c == '#':
			lineComm = true

		case c == '/' && i+1 < len(script) && script[i+1] == '*':
			blockComm = true
			i++

		case c == '\'' || c == '"' || c == '`':
			quote = c
			current.WriteByte(c)

		case c == ';':
			flush()

		default:
			current.WriteByte(c)
		}
	}

	if quote != 0 {
		return nil, fmt.Errorf("mysql: unterminated %c-quoted literal", quote)
	}
	if blockComm {
		return nil, fmt.Errorf("mysql: unterminated block comment")
	}

	flush()

	return out, nil
}
