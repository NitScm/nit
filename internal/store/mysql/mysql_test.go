package mysql_test

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/store/mysql"
	"github.com/NitScm/nit/internal/store/sqlmigrate"
	"github.com/NitScm/nit/migrations"
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

	storetest.Run(t, freshStore(dsn))
}

// TestPrunerConformance runs the retention suite. On this backend a purge has
// to drop and recreate a trigger rather than disable one, so "the guard is put
// back" is an assertion about DDL that cannot roll back.
func TestPrunerConformance(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("NIT_TEST_MYSQL not set")
	}

	storetest.RunPruner(t, freshStore(dsn))
}

// freshStore returns a factory that empties the database before each case.
func freshStore(dsn string) storetest.Factory {
	return func(t *testing.T) store.Store {
		ctx := context.Background()

		s, err := mysql.Open(ctx, dsn)
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { s.Close() })

		reset(t, ctx, s.DB())

		return s
	}
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
		"audit_log", "artifacts", "partition_leases", "tasks", "sync_points",
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

// The assertion the conformance suite cannot make: after a purge, the table
// refuses DELETE again.
//
// GuardsWereMissing says the prune believed it restored the trigger. This says
// the database agrees, which is a different claim — and on this backend the
// restoration is DDL that cannot be rolled back, so it is the one worth making.
func TestPruneLeavesTheAuditTableRefusingDelete(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("NIT_TEST_MYSQL not set")
	}

	ctx := context.Background()
	s := freshStore(dsn)(t).(*mysql.Store)

	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	for _, at := range []time.Time{cutoff.Add(-time.Hour), cutoff.Add(time.Hour)} {
		if err := s.Audit().Append(ctx, &store.AuditRecord{
			TenantID:      policy.DefaultTenant,
			OccurredAt:    at,
			ActorLabel:    "operator",
			Action:        "push.applied",
			PolicyVersion: "sha256:test",
		}); err != nil {
			t.Fatalf("append: %v", err)
		}
	}

	if _, err := s.PruneAudit(ctx, cutoff, 10); err != nil {
		t.Fatalf("PruneAudit: %v", err)
	}

	if _, err := s.DB().ExecContext(ctx, `DELETE FROM audit_log`); err == nil {
		t.Fatal("DELETE succeeded after a purge; the append-only guard was not restored")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("error = %v; the restored trigger must name the reason", err)
	}

	// And UPDATE was never lifted in the first place: a purge removes, it does
	// not open a window for rewriting.
	if _, err := s.DB().ExecContext(ctx, `UPDATE audit_log SET action = 'rewritten'`); err == nil {
		t.Error("UPDATE succeeded; only the DELETE guard may ever come off")
	}

	var remaining int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_log`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Errorf("%d records remain, want the one after the cutoff", remaining)
	}
}

// A prune that found the guard already gone has to say so, because between the
// interrupted purge and this one the table proved nothing.
func TestPruneReportsAnAbsentGuard(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_MYSQL")
	if dsn == "" {
		t.Skip("NIT_TEST_MYSQL not set")
	}

	ctx := context.Background()
	s := freshStore(dsn)(t).(*mysql.Store)

	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	// A row that outlives the cutoff, so the DELETE below has something to fire
	// the trigger on. A row trigger does not run on an empty table, which is
	// how an earlier version of this test passed against a missing guard.
	if err := s.Audit().Append(ctx, &store.AuditRecord{
		TenantID:      policy.DefaultTenant,
		OccurredAt:    cutoff.Add(time.Hour),
		ActorLabel:    "operator",
		Action:        "push.applied",
		PolicyVersion: "sha256:test",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	// Exactly what a hard kill mid-purge leaves behind.
	if _, err := s.DB().ExecContext(ctx, `DROP TRIGGER audit_log_no_delete`); err != nil {
		t.Fatalf("drop trigger: %v", err)
	}

	result, err := s.PruneAudit(ctx, cutoff, 10)
	if err != nil {
		t.Fatalf("PruneAudit: %v", err)
	}

	if !result.GuardsWereMissing {
		t.Error("the prune found no guard and did not report it")
	}

	// And it left one behind, which is the other half of being useful here.
	if _, err := s.DB().ExecContext(ctx, `DELETE FROM audit_log`); err == nil {
		t.Error("DELETE still succeeds; the prune did not restore the guard it found missing")
	}
}

// The trigger a purge recreates must be the trigger the migration created.
// Restoring a weaker guard than the one lifted would be worse than failing.
func TestTheRestoredTriggerMatchesTheMigration(t *testing.T) {
	loaded, err := sqlmigrate.Load(migrations.MySQL)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	var fromMigration string

	for _, m := range loaded {
		statements, err := mysql.SplitStatements(m.Up)
		if err != nil {
			t.Fatalf("SplitStatements: %v", err)
		}

		for _, statement := range statements {
			if strings.Contains(statement, "CREATE TRIGGER audit_log_no_delete") {
				fromMigration = statement
			}
		}
	}

	if fromMigration == "" {
		t.Fatal("no CREATE TRIGGER audit_log_no_delete in the embedded migrations")
	}

	if normalize(fromMigration) != normalize(mysql.AuditNoDeleteTrigger) {
		t.Errorf("the trigger a purge restores differs from the one the migration creates:\n"+
			"migration: %s\n\nrestored:  %s", fromMigration, mysql.AuditNoDeleteTrigger)
	}
}

// normalize collapses whitespace, so the comparison is about the statement
// rather than about indentation.
func normalize(s string) string { return strings.Join(strings.Fields(s), " ") }
