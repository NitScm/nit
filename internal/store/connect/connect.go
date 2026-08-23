// Package connect opens the store backend a DSN names.
//
// It exists so that "which database is this?" is decided in one place. Four
// binaries open a store and each would otherwise have to know the answer, which
// is how a deployment ends up with a server on one backend and a worker on
// another.
package connect

import (
	"context"
	"fmt"
	"strings"

	"github.com/NitScm/nit/internal/store/mysql"
	"github.com/NitScm/nit/internal/store/postgres"
	"github.com/NitScm/nit/internal/store/sqlmigrate"
	"github.com/NitScm/nit/migrations"
	"github.com/NitScm/nit/pkg/store"
)

// Backend names a supported database engine.
type Backend string

const (
	Postgres Backend = "postgres"
	MySQL    Backend = "mysql"
)

// Detect reports which backend a DSN names.
//
// PostgreSQL is the default for an unrecognised string only in the sense that
// it is what the error suggests: guessing would produce a connection failure
// three layers from the mistake.
func Detect(dsn string) (Backend, error) {
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		return Postgres, nil
	case mysql.IsDSN(dsn):
		return MySQL, nil
	}

	return "", fmt.Errorf("cannot tell which database %q names; "+
		"use postgres://user:password@host:5432/database "+
		"or user:password@tcp(host:3306)/database for MySQL and MariaDB",
		redact(dsn))
}

// Open connects to the backend the DSN names.
func Open(ctx context.Context, dsn string) (store.Store, error) {
	backend, err := Detect(dsn)
	if err != nil {
		return nil, err
	}

	switch backend {
	case Postgres:
		return postgres.Open(ctx, dsn)
	case MySQL:
		return mysql.Open(ctx, dsn)
	}

	return nil, fmt.Errorf("unsupported backend %q", backend)
}

// Migrations returns the embedded schema for a backend.
func Migrations(backend Backend) ([]sqlmigrate.Migration, error) {
	switch backend {
	case Postgres:
		return sqlmigrate.Load(migrations.Postgres)
	case MySQL:
		return sqlmigrate.Load(migrations.MySQL)
	}

	return nil, fmt.Errorf("unsupported backend %q", backend)
}

// Migrate applies every pending migration for the DSN's backend.
//
// The caller owns nothing afterwards: the connection is opened and closed here,
// because migrating is a one-shot operation and leaving a pool open around it
// only invites someone to reuse it.
func Migrate(ctx context.Context, dsn string) (applied int, err error) {
	backend, err := Detect(dsn)
	if err != nil {
		return 0, err
	}

	loaded, err := Migrations(backend)
	if err != nil {
		return 0, err
	}

	switch backend {
	case Postgres:
		s, err := postgres.Open(ctx, dsn)
		if err != nil {
			return 0, err
		}
		defer s.Close()

		return postgres.Migrate(ctx, s.Pool(), loaded)

	case MySQL:
		s, err := mysql.Open(ctx, dsn)
		if err != nil {
			return 0, err
		}
		defer s.Close()

		return mysql.Migrate(ctx, s.DB(), loaded)
	}

	return 0, fmt.Errorf("unsupported backend %q", backend)
}

// redact removes a password from a DSN of either shape, so an error message
// about a malformed DSN does not publish the credential inside it.
func redact(dsn string) string {
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}

	start := 0
	if scheme := strings.Index(dsn, "://"); scheme >= 0 && scheme < at {
		start = scheme + 3
	}

	credentials := dsn[start:at]
	if colon := strings.Index(credentials, ":"); colon >= 0 {
		credentials = credentials[:colon] + ":***"
	}

	return dsn[:start] + credentials + dsn[at:]
}
