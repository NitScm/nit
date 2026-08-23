package postgres_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/store"
	"github.com/NitScm/nit/internal/store/postgres"
	"github.com/NitScm/nit/internal/store/storetest"
	"github.com/NitScm/nit/pkg/policy"
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
		//
		// audit_log refuses to be emptied — a trigger raises on UPDATE, DELETE
		// and TRUNCATE alike, because TRUNCATE would otherwise bypass the row
		// trigger in one word. Disabling it here is exactly the deliberate act
		// the design intends for a genuine purge, and doing it in a test is the
		// cheapest possible proof that the escape hatch works.
		if _, err := s.Pool().Exec(ctx,
			`ALTER TABLE audit_log DISABLE TRIGGER audit_log_append_only_truncate`); err != nil {
			t.Fatalf("disable append-only trigger: %v", err)
		}

		_, err = s.Pool().Exec(ctx, `
			TRUNCATE audit_log, artifacts, tasks, sync_points,
			         repositories, workspaces, sessions, users
			RESTART IDENTITY CASCADE`)
		if err != nil {
			t.Fatalf("truncate: %v", err)
		}

		if _, err := s.Pool().Exec(ctx,
			`ALTER TABLE audit_log ENABLE TRIGGER audit_log_append_only_truncate`); err != nil {
			t.Fatalf("re-enable append-only trigger: %v", err)
		}

		return s
	})
}

// The audit trail's guarantee is that history cannot be rewritten. What
// changed in migration 0002 is that it now says so instead of pretending to
// comply: the rules it replaced answered a DELETE with "DELETE 0" and left
// every row in place, so an operator purging records was told it worked.
func TestAuditLogRefusesToBeRewritten(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("NIT_TEST_POSTGRES not set")
	}

	ctx := context.Background()

	s, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	if err := s.Audit().Append(ctx, &store.AuditRecord{
		TenantID:      policy.DefaultTenant,
		OccurredAt:    time.Now().UTC(),
		ActorLabel:    "maya",
		Action:        "push.denied_path",
		PolicyVersion: "sha256:test",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	var before int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&before); err != nil {
		t.Fatalf("count: %v", err)
	}

	for _, tc := range []struct{ name, statement string }{
		{"delete", `DELETE FROM audit_log`},
		{"update", `UPDATE audit_log SET action = 'rewritten'`},
		{"truncate", `TRUNCATE audit_log`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Pool().Exec(ctx, tc.statement); err == nil {
				t.Fatalf("%s succeeded; append-only must refuse it out loud", tc.name)
			} else if !strings.Contains(err.Error(), "append-only") {
				t.Errorf("error = %v; it must name the reason", err)
			}
		})
	}

	// Refusing has to mean not doing it, which is the half the old rules got
	// right and the half a loud failure could still get wrong.
	var after int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&after); err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != before {
		t.Errorf("row count went from %d to %d", before, after)
	}
}
