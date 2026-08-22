package bootstrap_test

import (
	"context"
	"testing"

	"github.com/NitScm/nit/internal/bootstrap"
	"github.com/NitScm/nit/internal/store/memory"
	"github.com/NitScm/nit/pkg/policy"
)

func TestReconcilePolicy(t *testing.T) {
	ctx := context.Background()
	st := memory.New()

	compiled, err := policy.Compile(policy.Spec{
		Version: "v1",
		Users:   []policy.User{{ID: "alice", Email: "alice@example.com"}},
		Repositories: []policy.Repository{
			{ID: "backend-api", Remote: "https://example.com/r.git", Forge: "github"},
		},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	if err := bootstrap.ReconcilePolicy(ctx, st, compiled, policy.DefaultTenant); err != nil {
		t.Fatalf("ReconcilePolicy: %v", err)
	}

	repo, err := st.Repositories().ByPolicyID(ctx, policy.DefaultTenant, "backend-api")
	if err != nil {
		t.Fatalf("ByPolicyID: %v", err)
	}
	if repo.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q, want the fallback %q", repo.DefaultBranch, "main")
	}

	// Reconciling twice must not duplicate anything: it runs on every start-up
	// and on every policy reload.
	if err := bootstrap.ReconcilePolicy(ctx, st, compiled, policy.DefaultTenant); err != nil {
		t.Fatalf("ReconcilePolicy again: %v", err)
	}

	repos, err := st.Repositories().List(ctx, policy.DefaultTenant)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(repos) != 1 {
		t.Errorf("got %d repositories after two reconciles, want 1", len(repos))
	}
}

// A token can only be issued to someone the bundle declares, so a typo produces
// an error rather than a credential for an account that authorizes nothing.
func TestReconcileUserRequiresABundleEntry(t *testing.T) {
	ctx := context.Background()
	st := memory.New()

	compiled, err := policy.Compile(policy.Spec{
		Version: "v1",
		Users:   []policy.User{{ID: "alice", Email: "alice@example.com"}},
	})
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	user, err := bootstrap.ReconcileUser(ctx, st, compiled, policy.DefaultTenant, "alice")
	if err != nil {
		t.Fatalf("ReconcileUser: %v", err)
	}
	if user.Email != "alice@example.com" {
		t.Errorf("Email = %q", user.Email)
	}

	if _, err := bootstrap.ReconcileUser(ctx, st, compiled, policy.DefaultTenant, "mallory"); err == nil {
		t.Error("a user absent from the bundle must not get a row")
	}
}
