package server_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/NitScm/nit/pkg/policy"
)

// With nothing configured for the tenant, the deployment-wide list decides.
// That is every self-hosted deployment, and it must not change.
func TestWithoutATenantListTheConfiguredOneDecides(t *testing.T) {
	f := newAdminFixture(t)

	response := f.do(http.MethodGet, "/v1/admin/stats", f.adminToken, nil)
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want the configured admin group to still work", response.StatusCode)
	}
}

// A tenant's own list replaces the configured one. This is what stops a hosted
// control plane granting one customer's administrators access to another's
// operations API.
func TestATenantsOwnListReplacesTheConfiguredOne(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()

	// A list naming a group the admin is not in. The configured list still
	// names one they are — so if the tenant's list is ignored, this passes and
	// proves nothing.
	if err := f.store.Tenants().SetAdminGroups(ctx, policy.DefaultTenant,
		[]policy.GroupID{"nobody-is-in-this"}); err != nil {
		t.Fatalf("SetAdminGroups: %v", err)
	}

	response := f.do(http.MethodGet, "/v1/admin/stats", f.adminToken, nil)
	defer response.Body.Close()

	// 404 rather than 403: the existence of an operations API is not something
	// an ordinary developer needs confirmed.
	if response.StatusCode != http.StatusNotFound {
		t.Errorf("status = %d, want 404; the tenant's list was ignored", response.StatusCode)
	}
}

// And it grants, not only refuses: a group named by the tenant works even
// though the configured list does not name it.
func TestATenantsOwnListGrants(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()

	if err := f.store.Tenants().SetAdminGroups(ctx, policy.DefaultTenant,
		[]policy.GroupID{"devs"}); err != nil {
		t.Fatalf("SetAdminGroups: %v", err)
	}

	// f.token belongs to an ordinary developer, who is in "devs" and not in the
	// configured admin group.
	response := f.do(http.MethodGet, "/v1/admin/stats", f.token, nil)
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want the tenant's list to grant access", response.StatusCode)
	}
}

// Writing the list replaces it rather than merging, so removing an
// administrator is one command instead of two.
func TestSettingTheListReplacesIt(t *testing.T) {
	f := newAdminFixture(t)
	ctx := context.Background()

	if err := f.store.Tenants().SetAdminGroups(ctx, policy.DefaultTenant,
		[]policy.GroupID{"devs", "platform"}); err != nil {
		t.Fatalf("SetAdminGroups: %v", err)
	}
	if err := f.store.Tenants().SetAdminGroups(ctx, policy.DefaultTenant,
		[]policy.GroupID{"platform"}); err != nil {
		t.Fatalf("SetAdminGroups: %v", err)
	}

	groups, err := f.store.Tenants().AdminGroups(ctx, policy.DefaultTenant)
	if err != nil {
		t.Fatalf("AdminGroups: %v", err)
	}

	if len(groups) != 1 || groups[0] != "platform" {
		t.Errorf("groups = %v, want the second write to have replaced the first", groups)
	}
}
