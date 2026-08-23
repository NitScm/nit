package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// RunPruner executes the retention suite against an implementation of
// store.AuditPruner.
//
// It is a separate entry point from Run because pruning is an operator
// capability rather than part of the Store contract: a backend may implement it
// or not, and a caller holding a Store cannot reach it either way.
//
// What these assertions protect is narrow and worth stating. A purge is the one
// operation that removes evidence, so it has to remove exactly what was asked
// for, leave everything else untouched, and put back the protection it lifted —
// on every backend, whether lifting it was a transaction or a schema change.
func RunPruner(t *testing.T, newStore Factory) {
	t.Helper()

	t.Run("RemovesOnlyWhatPrecedesTheCutoff", func(t *testing.T) {
		testPruneRespectsTheCutoff(t, newStore)
	})
	t.Run("SurvivesMoreRecordsThanOneBatch", func(t *testing.T) {
		testPruneBatches(t, newStore)
	})
	t.Run("RestoresTheAppendOnlyGuard", func(t *testing.T) {
		testPruneRestoresTheGuard(t, newStore)
	})
	t.Run("PruningNothingIsNotAnError", func(t *testing.T) {
		testPruneOfNothing(t, newStore)
	})
}

// pruner returns the store as an AuditPruner, skipping when it is not one.
func pruner(t *testing.T, s store.Store) store.AuditPruner {
	t.Helper()

	p, ok := s.(store.AuditPruner)
	if !ok {
		t.Skipf("%T does not implement store.AuditPruner", s)
	}

	return p
}

// appendAt writes one audit record at a given instant.
func appendAt(t *testing.T, s store.Store, at time.Time, action string) {
	t.Helper()

	err := s.Audit().Append(context.Background(), &store.AuditRecord{
		TenantID:      policy.DefaultTenant,
		OccurredAt:    at,
		ActorLabel:    "operator",
		Action:        action,
		PolicyVersion: "sha256:test",
	})
	if err != nil {
		t.Fatalf("append %s: %v", action, err)
	}
}

func countAudit(t *testing.T, s store.Store) int {
	t.Helper()

	records, err := s.Audit().Query(context.Background(), store.AuditQuery{
		Tenant: policy.DefaultTenant,
		Limit:  10000,
	})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	return len(records)
}

// The cutoff is exclusive, and a record exactly on it survives. Which side the
// boundary falls on is not a detail: a retention period of "90 days" applied
// with the wrong comparison deletes a day it was meant to keep, and there is no
// undo.
func testPruneRespectsTheCutoff(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)
	p := pruner(t, s)

	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	appendAt(t, s, cutoff.Add(-48*time.Hour), "old.two-days-before")
	appendAt(t, s, cutoff.Add(-time.Second), "old.one-second-before")
	appendAt(t, s, cutoff, "kept.exactly-on-the-cutoff")
	appendAt(t, s, cutoff.Add(time.Second), "kept.one-second-after")

	result, err := p.PruneAudit(ctx, cutoff, 100)
	if err != nil {
		t.Fatalf("PruneAudit: %v", err)
	}

	if result.Removed != 2 {
		t.Errorf("Removed = %d, want the 2 records before the cutoff", result.Removed)
	}

	records, err := s.Audit().Query(ctx, store.AuditQuery{Tenant: policy.DefaultTenant, Limit: 100})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	if len(records) != 2 {
		t.Fatalf("%d records remain, want 2", len(records))
	}

	for _, r := range records {
		if r.OccurredAt.Before(cutoff) {
			t.Errorf("a record from %s survived a cutoff of %s", r.OccurredAt, cutoff)
		}
		if r.Action == "old.one-second-before" || r.Action == "old.two-days-before" {
			t.Errorf("%s should have been removed", r.Action)
		}
	}
}

// A retention sweep covers more rows than one batch, and the loop that walks
// them is where an implementation stops early or never stops at all.
func testPruneBatches(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)
	p := pruner(t, s)

	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	const old = 25

	for i := range old {
		appendAt(t, s, cutoff.Add(-time.Duration(i+1)*time.Hour), "old.record")
	}

	appendAt(t, s, cutoff.Add(time.Hour), "kept.record")

	// A batch size that divides neither the old count nor it plus one, so a
	// loop that mishandles its last partial batch is caught.
	result, err := p.PruneAudit(ctx, cutoff, 7)
	if err != nil {
		t.Fatalf("PruneAudit: %v", err)
	}

	if result.Removed != old {
		t.Errorf("Removed = %d, want %d", result.Removed, old)
	}
	if got := countAudit(t, s); got != 1 {
		t.Errorf("%d records remain, want the single one after the cutoff", got)
	}
}

// A purge that left the table writable would be worse than one that failed:
// the failure is visible and the open door is not.
func testPruneRestoresTheGuard(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)
	p := pruner(t, s)

	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)

	appendAt(t, s, cutoff.Add(-time.Hour), "old.record")
	appendAt(t, s, cutoff.Add(time.Hour), "kept.record")

	result, err := p.PruneAudit(ctx, cutoff, 10)
	if err != nil {
		t.Fatalf("PruneAudit: %v", err)
	}

	// The guard was in place before this ran, so nothing should have been
	// found missing.
	if result.GuardsWereMissing {
		t.Error("the prune reported the append-only guard already absent on a fresh store")
	}

	// A second prune with a cutoff that matches nothing must still find the
	// guard in place: that is the observable proof the first one put it back.
	second, err := p.PruneAudit(ctx, cutoff, 10)
	if err != nil {
		t.Fatalf("second PruneAudit: %v", err)
	}
	if second.GuardsWereMissing {
		t.Error("the first prune did not restore the append-only guard")
	}
	if second.Removed != 0 {
		t.Errorf("the second prune removed %d records; the first left work undone", second.Removed)
	}
}

func testPruneOfNothing(t *testing.T, newStore Factory) {
	ctx := context.Background()
	s := newStore(t)
	p := pruner(t, s)

	appendAt(t, s, time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), "kept.record")

	result, err := p.PruneAudit(ctx, time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC), 10)
	if err != nil {
		t.Fatalf("PruneAudit: %v", err)
	}

	if result.Removed != 0 {
		t.Errorf("Removed = %d, want 0", result.Removed)
	}
	if got := countAudit(t, s); got != 1 {
		t.Errorf("%d records remain, want 1", got)
	}
}
