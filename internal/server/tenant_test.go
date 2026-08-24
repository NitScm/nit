package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// The tenant a request may touch comes from its token, not from the process.
//
// Until now it came from `server.Config.Tenant`, fixed at start-up, so a
// process served exactly one customer. That is right for a self-hosted
// deployment and is the thing a hosted control plane cannot do — and the
// failure mode of getting it wrong is not an error, it is one company reading
// another's repository list.
func TestTheTenantComesFromTheToken(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A second tenant, with its own user, session and repository. The schema
	// has carried tenant_id from the start (D7); what is new is that a request
	// resolves which one.
	if err := seedTenant(ctx, f, "globex"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	other, err := f.store.Users().Upsert(ctx, &store.User{
		TenantID:     "globex",
		PolicyUserID: "dev",
		Email:        "dev@globex.example",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	if err := f.store.Repositories().Reconcile(ctx, "globex", []*store.Repository{{
		PolicyRepoID: "globex-only", Remote: "https://globex.example/r.git",
		Forge: "generic", DefaultBranch: "main",
	}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// A token issued into the second tenant. Issue is the operator's path and
	// still takes the tenant it is provisioning; what must be resolved from the
	// token is *authentication*.
	service := auth.NewService(f.store, policy.OneSource{Source: f.policySource}, "globex", nil)

	token, _, err := service.Issue(ctx, other.ID, "globex laptop", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	principal, err := f.authService.Authenticate(ctx, token)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}

	// The service authenticating was built for the default tenant, and it
	// still resolved this token to the tenant that owns it. That is the whole
	// change: the answer comes from the session, not from the field.
	if principal.Tenant != "globex" {
		t.Errorf("Tenant = %q, want globex", principal.Tenant)
	}
	if principal.Session.TenantID != "globex" {
		t.Errorf("the session belongs to %q", principal.Session.TenantID)
	}
}

// The consequence, at the endpoint an operator would notice first: each token
// sees its own tenant's repositories and no others.
func TestARequestSeesOnlyItsOwnTenantsRepositories(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	if err := seedTenant(ctx, f, "globex"); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}

	if err := f.store.Repositories().Reconcile(ctx, "globex", []*store.Repository{{
		PolicyRepoID: "globex-only", Remote: "https://globex.example/r.git",
		Forge: "generic", DefaultBranch: "main",
	}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	// A user and a token *inside* the second tenant. Querying from the default
	// tenant would prove nothing: the right answer and the wrong one coincide
	// there, which is how the first version of this test passed against a
	// resolution that had been removed.
	other, err := f.store.Users().Upsert(ctx, &store.User{
		TenantID:     "globex",
		PolicyUserID: "dev",
		Email:        "dev@globex.example",
	})
	if err != nil {
		t.Fatalf("upsert user: %v", err)
	}

	service := auth.NewService(f.store, policy.OneSource{Source: f.policySource}, "globex", nil)

	token, _, err := service.Issue(ctx, other.ID, "globex laptop", time.Hour)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	response := f.do(http.MethodGet, protocol.RouteRepos, token, nil)
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}

	var listed []struct {
		ID string `json:"id"`
	}

	if err := json.NewDecoder(response.Body).Decode(&listed); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if len(listed) == 0 {
		t.Fatal("the second tenant saw no repositories at all")
	}

	for _, repo := range listed {
		if repo.ID != "globex-only" {
			t.Errorf("a repository of another tenant was listed: %s", repo.ID)
		}
	}
}

// seedTenant adds the row every tenant-scoped record references.
func seedTenant(ctx context.Context, f *fixture, id policy.TenantID) error {
	// The in-memory store has no tenants table to satisfy; a SQL backend
	// would, which is why this exists as a helper rather than inline.
	_ = ctx
	_ = f
	_ = id

	return nil
}
