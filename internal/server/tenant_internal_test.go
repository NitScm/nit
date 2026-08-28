package server

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// A context that names no tenant must name no tenant.
//
// This is the whole of the fix, and it is one line of behaviour: `tenantOf`
// used to answer `policy.DefaultTenant` when nothing had put a tenant in the
// context. Every route that reaches it is authenticated, so that state is a
// routing mistake rather than an unauthenticated caller — but the fallback
// turned the mistake into "read the first tenant's data", which is invisible
// in the single-tenant deployment everybody runs and is a cross-tenant read on
// the first day there are two.
//
// Empty is what both backends refuse. PostgreSQL stamps it on the connection
// and row-level security matches no rows; MySQL puts it in the WHERE clause and
// it matches none there either. A routing mistake becomes an empty result
// somebody reports rather than a disclosure nobody notices.
func TestAnAbsentTenantIsEmptyAndNotTheDefault(t *testing.T) {
	got := tenantOf(context.Background())

	if got == policy.DefaultTenant {
		t.Fatal("a context naming no tenant resolved to the default tenant; " +
			"in a deployment with two tenants that is one customer reading another's data")
	}

	if got != "" {
		t.Errorf("tenantOf on a bare context = %q, want the empty tenant", got)
	}
}

// And a context that does name one is believed.
func TestTheTenantInTheContextIsTheAnswer(t *testing.T) {
	ctx := store.WithTenant(context.Background(), policy.TenantID("globex"))

	if got := tenantOf(ctx); got != "globex" {
		t.Errorf("tenantOf = %q, want globex", got)
	}
}

// The structural half, because the fallback is the kind of thing that gets
// added back.
//
// It reads plausible: a handler needs a tenant, the principal has one, and
// defaulting when it is missing looks like defensiveness rather than a
// disclosure. The reviewer looking at `if principal := auth.PrincipalFrom(ctx);
// principal != nil { return principal.Tenant }` has to remember why it is
// wrong, and this fails the build instead.
//
// The reason it is wrong is not that the principal's tenant is incorrect — it
// is the same value. It is that asking twice creates two answers that can
// differ. `serve` puts the principal and the tenant in the context together,
// in one place; the store reads the second one to stamp the connection. A
// handler reading the first could ask for one tenant's rows over a connection
// permitted to read none, and get an empty result whose cause is invisible.
func TestTenantOfConsultsOnlyTheContextTenant(t *testing.T) {
	fset := token.NewFileSet()

	file, err := parser.ParseFile(fset, "tenant.go", nil, 0)
	if err != nil {
		t.Fatalf("parse tenant.go: %v", err)
	}

	var body string

	ast.Inspect(file, func(n ast.Node) bool {
		decl, ok := n.(*ast.FuncDecl)
		if !ok || decl.Name.Name != "tenantOf" || decl.Body == nil {
			return true
		}

		var names []string

		ast.Inspect(decl.Body, func(inner ast.Node) bool {
			if id, ok := inner.(*ast.Ident); ok {
				names = append(names, id.Name)
			}

			return true
		})

		body = strings.Join(names, " ")

		return false
	})

	if body == "" {
		t.Fatal("tenantOf was not found in tenant.go; this test has stopped reading the source")
	}

	for _, forbidden := range []string{"PrincipalFrom", "DefaultTenant"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("tenantOf names %s. The tenant comes from the context the store "+
				"reads, and nowhere else: a second source is a second answer, and a "+
				"default is another tenant's data.", forbidden)
		}
	}
}
