package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/NitScm/nit/pkg/store"
)

// The limit of this layer, asserted rather than discovered later.
//
// A worker drains one queue for every tenant: Claim takes the oldest
// dispatchable task, whoever it belongs to. There is no tenant to stamp on the
// connection, because finding out whose task it is *is* the query.
//
// So under row-level security a worker sees an empty queue. That is not a bug
// in the policies and not a bug in the worker — it is what "the control plane's
// request path is protected" costs, and a deployment has to answer it
// deliberately: either the worker connects as a role with BYPASSRLS, being
// trusted infrastructure rather than a tenant, or Claim becomes per-tenant and
// the single queue stops being single.
//
// If this test ever fails, one of those two happened and the reasoning above
// needs rewriting.
func TestAWorkerSeesNoQueueUnderRowSecurity(t *testing.T) {
	s, _ := unprivilegedStore(t)

	seedTenantData(t, "acme")

	// A claim as a worker makes it: no tenant, because it does not know one.
	_, err := s.Tasks().Claim(context.Background(), store.ClaimOptions{
		Holder:   "worker-1",
		LeaseFor: time.Minute,
		Now:      time.Now().UTC(),
	})

	if !errors.Is(err, store.ErrNoTask) {
		t.Fatalf("Claim = %v, want ErrNoTask; a worker cannot run under these policies "+
			"and the deployment has to say how it will", err)
	}
}
