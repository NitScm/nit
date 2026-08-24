package worker

import (
	"context"
	"testing"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// The tenant of the work has to reach the store, not just the query arguments:
// a backend enforcing row-level security reads it off the connection, so a
// worker that skipped this would see an empty database and quietly do nothing.
func TestATasksTenantReachesTheStore(t *testing.T) {
	ctx := taskContext(context.Background(), &store.Task{TenantID: "acme"})

	if got := store.TenantFrom(ctx); got != "acme" {
		t.Errorf("the store would see tenant %q, want acme", got)
	}
}

// A task with no tenant is left alone rather than defaulted: every task has
// one, and inventing a tenant would run the work against somebody's data
// instead of failing.
func TestATaskWithNoTenantIsNotGivenOne(t *testing.T) {
	ctx := taskContext(context.Background(), &store.Task{})

	if got := store.TenantFrom(ctx); got != "" {
		t.Errorf("a task with no tenant was stamped with %q", got)
	}
}

// And an existing tenant on the context is replaced by the task's, because a
// runner handles tasks of several tenants in sequence on one context tree.
func TestTheTasksTenantWins(t *testing.T) {
	ctx := store.WithTenant(context.Background(), policy.DefaultTenant)
	ctx = taskContext(ctx, &store.Task{TenantID: "globex"})

	if got := store.TenantFrom(ctx); got != "globex" {
		t.Errorf("tenant = %q, want the task's", got)
	}
}
