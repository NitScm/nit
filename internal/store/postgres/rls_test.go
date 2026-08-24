package postgres_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/NitScm/nit/internal/store/postgres"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// Row-level security fails silently, so it is tested under the only conditions
// where it does anything: a role that neither owns the tables nor is a
// superuser.
//
// Running these as the superuser the rest of the suite uses would pass while
// proving nothing — which is exactly the trap the migration's comment warns
// about, and the reason this file exists rather than a line in the main suite.
func unprivilegedStore(t *testing.T) (*postgres.Store, string) {
	t.Helper()

	dsn := os.Getenv("NIT_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("NIT_TEST_POSTGRES not set")
	}

	ctx := context.Background()

	admin := freshStore(dsn)(t).(*postgres.Store)

	// Created from the privileged connection the suite already has. Idempotent,
	// because a test that only works on a clean database is a test that stops
	// being run.
	for _, statement := range []string{
		`DROP OWNED BY nit_rls_test`,
		`DROP ROLE IF EXISTS nit_rls_test`,
		`CREATE ROLE nit_rls_test LOGIN PASSWORD 'nit'`,
		`GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO nit_rls_test`,
		`GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA public TO nit_rls_test`,
		`GRANT USAGE ON SCHEMA public TO nit_rls_test`,
	} {
		// The drops fail on a database that has never run this; that is
		// expected and not worth branching on.
		_, _ = admin.Pool().Exec(ctx, statement)
	}

	unprivileged := strings.Replace(dsn, "postgres:postgres@", "nit_rls_test:nit@", 1)
	if unprivileged == dsn {
		t.Skipf("cannot derive an unprivileged DSN from %q", redact(dsn))
	}

	s, err := postgres.Open(ctx, unprivileged)
	if err != nil {
		t.Fatalf("open as nit_rls_test: %v", err)
	}
	t.Cleanup(func() { s.Close() })

	return s, unprivileged
}

// redact keeps a DSN out of a failure message.
func redact(dsn string) string {
	if at := strings.LastIndex(dsn, "@"); at >= 0 {
		return "…@" + dsn[at+1:]
	}

	return dsn
}

// The claim the migration makes, checked rather than asserted: under a role
// that RLS applies to, the policies are actually in force.
func TestRowSecurityIsEnforcedForAnOrdinaryRole(t *testing.T) {
	s, _ := unprivilegedStore(t)

	enforced, err := s.RowSecurityEnforced(context.Background())
	if err != nil {
		t.Fatalf("RowSecurityEnforced: %v", err)
	}
	if !enforced {
		t.Error("the policies do not apply to an ordinary role; they protect nothing")
	}
}

// And the honest half: it reports *false* for the superuser most deployments
// start out using, rather than claiming a protection that is not there.
func TestRowSecurityIsReportedAbsentForASuperuser(t *testing.T) {
	dsn := os.Getenv("NIT_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("NIT_TEST_POSTGRES not set")
	}

	s := freshStore(dsn)(t).(*postgres.Store)

	enforced, err := s.RowSecurityEnforced(context.Background())
	if err != nil {
		t.Fatalf("RowSecurityEnforced: %v", err)
	}
	if enforced {
		t.Error("row security was reported in force for a superuser, which bypasses it")
	}
}

// The property the whole layer exists for: a query with no tenant in its
// context reads nothing, rather than reading whoever's rows happen to be there.
func TestForgettingTheTenantReadsNothing(t *testing.T) {
	s, _ := unprivilegedStore(t)

	seedTenantData(t, "acme")

	// No store.WithTenant: this is the caller that forgot.
	repos, err := s.Repositories().List(context.Background(), "acme")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(repos) != 0 {
		t.Errorf("%d repositories were readable with no tenant stamped on the connection", len(repos))
	}
}

// With the tenant stamped, the same query works — otherwise the layer above
// would be broken rather than protected.
func TestStampingTheTenantMakesTheSameQueryWork(t *testing.T) {
	s, _ := unprivilegedStore(t)

	seedTenantData(t, "acme")

	ctx := store.WithTenant(context.Background(), "acme")

	repos, err := s.Repositories().List(ctx, "acme")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(repos) == 0 {
		t.Fatal("nothing was readable with the tenant stamped; the policy is too strict")
	}
}

// And the one that matters: a request stamped with one tenant cannot read
// another's rows even when the query asks for them.
//
// This is the failure the whole layer exists to catch — a caller that passed the
// wrong tenant, or passed one it should not have. The filter in the query would
// happily return the other tenant's rows; the policy does not.
func TestAStampedTenantCannotReadAnother(t *testing.T) {
	s, _ := unprivilegedStore(t)

	seedTenantData(t, "acme")
	seedTenantData(t, "globex")

	ctx := store.WithTenant(context.Background(), "acme")

	// The query asks for globex. The connection says acme. The policy wins.
	repos, err := s.Repositories().List(ctx, "globex")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	if len(repos) != 0 {
		t.Errorf("%d of another tenant's repositories were readable", len(repos))
	}
}

// A write is covered too. WITH CHECK is what stops a row being inserted into a
// tenant the connection is not stamped with — a read-only policy would leave
// the more damaging half open.
func TestAStampedTenantCannotWriteIntoAnother(t *testing.T) {
	s, _ := unprivilegedStore(t)

	seedTenantData(t, "acme")
	seedTenantData(t, "globex")

	ctx := store.WithTenant(context.Background(), "acme")

	err := s.Repositories().Reconcile(ctx, "globex", []*store.Repository{{
		PolicyRepoID: "planted", Remote: "https://example.com/x.git",
		Forge: "generic", DefaultBranch: "main",
	}})

	if err == nil {
		t.Fatal("a row was written into another tenant")
	}
	if errors.Is(err, store.ErrNotFound) {
		t.Errorf("error = %v; a policy violation should not look like a missing row", err)
	}
}

// seedTenantData writes a tenant and a repository through the privileged
// connection, which is what an operator provisioning a customer would use.
func seedTenantData(t *testing.T, tenant policy.TenantID) {
	t.Helper()

	dsn := os.Getenv("NIT_TEST_POSTGRES")
	ctx := context.Background()

	admin, err := postgres.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer admin.Close()

	if _, err := admin.Pool().Exec(ctx,
		`INSERT INTO tenants (id, name) VALUES ($1, $1) ON CONFLICT DO NOTHING`,
		string(tenant)); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}

	if err := admin.Repositories().Reconcile(ctx, tenant, []*store.Repository{{
		PolicyRepoID:  policy.RepoID(string(tenant) + "-repo"),
		Remote:        "https://example.com/r.git",
		Forge:         "generic",
		DefaultBranch: "main",
	}}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}
