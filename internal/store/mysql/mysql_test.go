package mysql_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/store/mysql"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
	"github.com/NitScm/nit/pkg/store/storetest"
)

// TestConformance runs the shared store suite against MySQL or MariaDB.
//
// It is skipped unless NIT_TEST_MYSQL names a database, because a unit test run
// must not require infrastructure. Set it to a DSN pointing at a database the
// test may empty:
//
//	NIT_TEST_MYSQL='root:nit@tcp(127.0.0.1:3307)/nit_test' go test ./internal/store/mysql/
//
// The same suite runs against PostgreSQL and the in-memory store. That is the
// whole argument for trusting this backend: the queue semantics are subtle
// enough that a second implementation is only safe if something proves it
// indistinguishable from the first.
func TestConformance(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("NIT_TEST_MYSQL not set")
	}

	storetest.Run(t, func(t *testing.T) store.Store {
		ctx := context.Background()

		s, err := mysql.Open(ctx, dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { s.Close() })

		reset(t, ctx, s.DB())

		return s
	})
}

// reset empties every table but tenants, whose default row the schema inserts
// once.
//
// TRUNCATE, which resets AUTO_INCREMENT so audit ordering assertions do not
// depend on what ran before. Note what makes that possible: on this backend
// TRUNCATE is *not* intercepted by the append-only triggers, because neither
// MySQL nor MariaDB fires a trigger for it. The PostgreSQL harness has to
// disable a trigger to do the same thing. Relying on the gap here is
// deliberate — it is the clearest place to see that the gap is real.
func reset(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer conn.Close()

	// Session-scoped, which is why this runs on one pinned connection rather
	// than through the pool.
	if _, err := conn.ExecContext(ctx, `SET FOREIGN_KEY_CHECKS = 0`); err != nil {
		t.Fatalf("disable foreign key checks: %v", err)
	}

	for _, table := range []string{
		"audit_log", "artifacts", "tasks", "sync_points",
		"repositories", "workspaces", "sessions", "users",
	} {
		if _, err := conn.ExecContext(ctx, `TRUNCATE TABLE `+table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
	}

	if _, err := conn.ExecContext(ctx, `SET FOREIGN_KEY_CHECKS = 1`); err != nil {
		t.Fatalf("re-enable foreign key checks: %v", err)
	}
}

// The audit trail's guarantee is that history cannot be rewritten. On this
// backend the guarantee is two thirds of a guarantee, and the test says so
// rather than testing only the part that passes.
func TestAuditLogRefusesToBeRewritten(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("NIT_TEST_MYSQL not set")
	}

	ctx := context.Background()

	s, err := mysql.Open(ctx, dsn)
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
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&before); err != nil {
		t.Fatalf("count: %v", err)
	}

	for _, tc := range []struct{ name, statement string }{
		{"delete", `DELETE FROM audit_log`},
		{"update", `UPDATE audit_log SET action = 'rewritten'`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.DB().ExecContext(ctx, tc.statement); err == nil {
				t.Fatalf("%s succeeded; append-only must refuse it out loud", tc.name)
			} else if !strings.Contains(err.Error(), "append-only") {
				t.Errorf("error = %v; it must name the reason", err)
			}
		})
	}

	// Refusing has to mean not doing it.
	var after int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&after); err != nil {
		t.Fatalf("count: %v", err)
	}
	if after != before {
		t.Errorf("row count went from %d to %d", before, after)
	}
}

// The gap, asserted so it cannot close by accident and go unnoticed, and cannot
// be forgotten while it is open.
//
// PostgreSQL refuses TRUNCATE with a statement-level trigger. Neither MySQL nor
// MariaDB fires any trigger on TRUNCATE, so the statement succeeds and the
// audit trail is gone. What holds the line on this backend is a privilege:
// TRUNCATE requires DROP, which the application account must not be granted.
//
// If this test ever fails, an engine has grown the ability to intercept
// TRUNCATE — which would be good news, and means migration 0002 and
// docs/CONFIGURATION.md should say something different.
func TestTruncateIsNotInterceptedOnThisBackend(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("NIT_TEST_MYSQL not set")
	}

	ctx := context.Background()

	s, err := mysql.Open(ctx, dsn)
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

	if _, err := s.DB().ExecContext(ctx, `TRUNCATE TABLE audit_log`); err != nil {
		t.Fatalf("TRUNCATE failed: %v\n"+
			"if an engine now intercepts it, migration 0002 and the deployment "+
			"guide both understate the guarantee and should be corrected", err)
	}

	var remaining int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Errorf("%d rows survived a TRUNCATE, which is not how TRUNCATE works", remaining)
	}
}

// A DSN that would silently shift every timestamp has to be refused rather than
// corrected: an operator who wrote loc=Local meant something by it.
func TestOpenRefusesANonUTCDSN(t *testing.T) {
	_, err := mysql.Open(context.Background(), "nit:secret@tcp(127.0.0.1:3306)/nit?loc=Local")
	if err == nil {
		t.Fatal("a DSN with loc=Local was accepted")
	}
	if !strings.Contains(err.Error(), "UTC") {
		t.Errorf("error = %v; it must name the reason", err)
	}
}

// The URL form is what someone writes after reading the PostgreSQL
// instructions. Saying so is worth more than "invalid DSN".
func TestOpenExplainsTheURLForm(t *testing.T) {
	_, err := mysql.Open(context.Background(), "mysql://nit:secret@127.0.0.1:3306/nit")
	if err == nil {
		t.Fatal("a URL-form DSN was accepted")
	}
	if !strings.Contains(err.Error(), "tcp(") {
		t.Errorf("error = %v; it must show the shape the driver wants", err)
	}
}

// A password must not reach a log or a terminal.
func TestRedactDSN(t *testing.T) {
	got := mysql.RedactDSN("nit:hunter2@tcp(db:3306)/nit")

	if strings.Contains(got, "hunter2") {
		t.Errorf("RedactDSN kept the password: %q", got)
	}
	if !strings.Contains(got, "nit:***@tcp(db:3306)/nit") {
		t.Errorf("RedactDSN = %q, want the host and user still readable", got)
	}
}
