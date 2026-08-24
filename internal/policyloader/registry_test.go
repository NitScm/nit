package policyloader_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/pkg/policy"
)

// Each tenant gets its own rules. That is the whole point: one process serving
// every customer the same bundle is what a hosted control plane cannot do.
func TestEachTenantGetsItsOwnBundle(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	writeConformanceBundle(t, filepath.Join(root, "acme"), true)
	writeConformanceBundle(t, filepath.Join(root, "globex"), false)

	registry := policyloader.NewRegistry(root, 0, quiet())
	defer registry.Close()

	acme, err := registry.For(ctx, "acme")
	if err != nil {
		t.Fatalf("For(acme): %v", err)
	}

	globex, err := registry.For(ctx, "globex")
	if err != nil {
		t.Fatalf("For(globex): %v", err)
	}

	if !canRead(t, acme.Current()) {
		t.Error("acme's bundle allows the read and the served one does not")
	}
	if canRead(t, globex.Current()) {
		t.Error("globex's bundle denies the read and the served one allows it")
	}
	if acme.Current().Version() == globex.Current().Version() {
		t.Error("two tenants share a version, so they share a bundle")
	}
}

// Asking twice returns the same bundle, or a caller holding one across a
// request would see it change under them.
func TestASecondLookupReturnsTheSameBundle(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	writeConformanceBundle(t, filepath.Join(root, "acme"), true)

	registry := policyloader.NewRegistry(root, 0, quiet())
	defer registry.Close()

	first, err := registry.For(ctx, "acme")
	if err != nil {
		t.Fatalf("For: %v", err)
	}

	second, err := registry.For(ctx, "acme")
	if err != nil {
		t.Fatalf("For: %v", err)
	}

	if first.Current() != second.Current() {
		t.Error("two lookups of one tenant returned different bundles")
	}
}

// A tenant with no bundle is an error, never an empty bundle. An empty one
// denies everything, which sounds safe and means a customer locked out of their
// own repositories with nothing saying why.
func TestAnUnknownTenantIsAnError(t *testing.T) {
	root := t.TempDir()

	writeConformanceBundle(t, filepath.Join(root, "acme"), true)

	registry := policyloader.NewRegistry(root, 0, quiet())
	defer registry.Close()

	source, err := registry.For(context.Background(), "nobody")
	if err == nil {
		t.Fatal("a tenant with no bundle was served one")
	}
	if source != nil {
		t.Error("an error came with a source")
	}
	if !strings.Contains(err.Error(), "nobody") {
		t.Errorf("error = %v; it must name the tenant", err)
	}
}

// The compatibility case, and the reason it is narrow: an existing deployment
// points policy.dir straight at a bundle, and that must keep working for the
// default tenant.
func TestTheDefaultTenantFallsBackToTheRoot(t *testing.T) {
	root := t.TempDir()

	writeConformanceBundle(t, root, true)

	registry := policyloader.NewRegistry(root, 0, quiet())
	defer registry.Close()

	source, err := registry.For(context.Background(), policy.DefaultTenant)
	if err != nil {
		t.Fatalf("the existing layout stopped working: %v", err)
	}
	if source.Current() == nil {
		t.Fatal("no bundle")
	}
}

// And the half that makes the fallback safe: another tenant must not inherit
// it. A missing bundle silently serving somebody else's rules is the one
// mistake this layer exists to make impossible.
func TestOnlyTheDefaultTenantFallsBack(t *testing.T) {
	root := t.TempDir()

	writeConformanceBundle(t, root, true)

	registry := policyloader.NewRegistry(root, 0, quiet())
	defer registry.Close()

	if _, err := registry.For(context.Background(), "acme"); err == nil {
		t.Fatal("a tenant with no bundle of its own was served the root's")
	}
}

// Concurrent first lookups must not produce two bundles for one tenant.
func TestConcurrentFirstLookups(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	writeConformanceBundle(t, filepath.Join(root, "acme"), true)

	registry := policyloader.NewRegistry(root, 0, quiet())
	defer registry.Close()

	const callers = 16

	results := make(chan policy.Source, callers)

	for range callers {
		go func() {
			source, err := registry.For(ctx, "acme")
			if err != nil {
				t.Error(err)
				results <- nil

				return
			}

			results <- source
		}()
	}

	var first policy.Source

	for range callers {
		got := <-results
		if got == nil {
			continue
		}

		if first == nil {
			first = got
			continue
		}

		if got.Current() != first.Current() {
			t.Error("concurrent lookups produced different bundles for one tenant")
		}
	}
}

// A watcher per tenant, started only when the registry was given an interval.
func TestAWatchedBundleReloads(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := filepath.Join(root, "acme")
	writeConformanceBundle(t, dir, true)

	registry := policyloader.NewRegistry(root, 10*time.Millisecond, quiet())
	defer registry.Close()

	source, err := registry.For(ctx, "acme")
	if err != nil {
		t.Fatalf("For: %v", err)
	}

	if !canRead(t, source.Current()) {
		t.Fatal("the fixture is wrong")
	}

	writeConformanceBundle(t, dir, false)

	deadline := time.Now().Add(3 * time.Second)

	for time.Now().Before(deadline) {
		if !canRead(t, source.Current()) {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Error("the bundle never reloaded")
}

// canRead asks the question the conformance bundle is built to answer.
func canRead(t *testing.T, p *policy.Policy) bool {
	t.Helper()

	subject, err := p.Subject("dev")
	if err != nil {
		t.Fatalf("Subject: %v", err)
	}

	return p.Evaluate(policy.Request{
		Repo: "repo", Ref: "refs/heads/main", Subject: subject,
		Path: "src/app.go", Action: policy.ActionRead,
	}).Allowed
}
