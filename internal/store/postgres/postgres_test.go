package postgres_test

import (
	"context"
	"os"
	"testing"

	"github.com/NitScm/nit/internal/store"
	"github.com/NitScm/nit/internal/store/postgres"
	"github.com/NitScm/nit/internal/store/storetest"
)

// TestConformance runs the shared store suite against PostgreSQL.
//
// It is skipped unless NIT_TEST_POSTGRES names a database, because a unit test
// run must not require infrastructure. Set it to a DSN pointing at a database
// the test may truncate:
//
//	NIT_TEST_POSTGRES='postgres://postgres:postgres@localhost:5432/nit_test' go test ./internal/store/postgres/
func TestConformance(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("NIT_TEST_POSTGRES not set")
	}

	storetest.Run(t, func(t *testing.T) store.Store {
		ctx := context.Background()

		s, err := postgres.Open(ctx, dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { s.Close() })

		// Each test starts from an empty database. RESTART IDENTITY resets the
		// audit sequence so ordering assertions do not depend on what ran
		// before.
		_, err = s.Pool().Exec(ctx, `
			TRUNCATE audit_log, artifacts, tasks, sync_points,
			         repositories, workspaces, sessions, users
			RESTART IDENTITY CASCADE`)
		if err != nil {
			t.Fatalf("truncate: %v", err)
		}

		return s
	})
}
