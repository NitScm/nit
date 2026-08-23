package postgres_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/store/postgres"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// After a purge the table refuses DELETE again — the claim GuardsWereMissing
// cannot make, since it reports what the prune believed rather than what the
// database enforces.
func TestPruneLeavesTheAuditTableRefusingDelete(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("NIT_TEST_POSTGRES not set")
	}

	ctx := context.Background()
	s := freshStore(dsn)(t).(*postgres.Store)

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

	result, err := s.PruneAudit(ctx, cutoff, 10)
	if err != nil {
		t.Fatalf("PruneAudit: %v", err)
	}
	if result.Removed != 1 {
		t.Errorf("Removed = %d, want 1", result.Removed)
	}

	// A row survives the cutoff, so the trigger has something to fire on: a row
	// trigger does not run on an empty table, and a test that forgot that would
	// pass against no guard at all.
	if _, err := s.Pool().Exec(ctx, `DELETE FROM audit_log`); err == nil {
		t.Fatal("DELETE succeeded after a purge; the append-only guard was not restored")
	} else if !strings.Contains(err.Error(), "append-only") {
		t.Errorf("error = %v; the restored trigger must name the reason", err)
	}

	if _, err := s.Pool().Exec(ctx, `UPDATE audit_log SET action = 'rewritten'`); err == nil {
		t.Error("UPDATE succeeded; a purge removes, it never opens a window for rewriting")
	}

	var remaining int
	if err := s.Pool().QueryRow(ctx, `SELECT count(*) FROM audit_log`).Scan(&remaining); err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 1 {
		t.Errorf("%d records remain, want the one after the cutoff", remaining)
	}
}

// TRUNCATE stays refused throughout. Only the row-level guard is ever lifted,
// and only for the length of one batch.
func TestPruneDoesNotTouchTheTruncateGuard(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("NIT_TEST_POSTGRES not set")
	}

	ctx := context.Background()
	s := freshStore(dsn)(t).(*postgres.Store)

	if err := s.Audit().Append(ctx, &store.AuditRecord{
		TenantID:      policy.DefaultTenant,
		OccurredAt:    time.Date(2026, 6, 1, 1, 0, 0, 0, time.UTC),
		ActorLabel:    "operator",
		Action:        "push.applied",
		PolicyVersion: "sha256:test",
	}); err != nil {
		t.Fatalf("append: %v", err)
	}

	if _, err := s.PruneAudit(ctx, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), 10); err != nil {
		t.Fatalf("PruneAudit: %v", err)
	}

	if _, err := s.Pool().Exec(ctx, `TRUNCATE audit_log`); err == nil {
		t.Error("TRUNCATE succeeded after a purge")
	}
}
