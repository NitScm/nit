package policyloader

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/NitScm/nit/pkg/policy"
)

// Registry keeps one bundle per tenant, each with its own version and its own
// reload.
//
// A single loader is right for a self-hosted deployment and is what a hosted
// control plane cannot use: one process would serve every customer the same
// rules. This is the tenant-keyed layer in front of it.
//
// # It is a cache, not a redesign
//
// Compilation is pure and a compiled bundle is immutable, so a tenant's bundle
// is loaded once, watched, and handed out by pointer. Nothing here participates
// in evaluation; it decides *which* bundle answers, never what it answers.
//
// # Where a tenant's bundle lives
//
// `<root>/<tenant>`, with one exception: the default tenant falls back to
// `<root>` itself when no `<root>/default` exists. That is what keeps every
// existing deployment working — `policy.dir` points straight at a bundle today,
// and moving it would be a migration nobody asked for to enable a feature they
// are not using.
type Registry struct {
	root string
	log  *slog.Logger

	// reload is the interval a newly created loader watches at. Zero means the
	// caller drives reloads itself.
	reload time.Duration

	mu      sync.Mutex
	loaders map[policy.TenantID]*Loader
	cancels map[policy.TenantID]context.CancelFunc
}

// NewRegistry returns a registry over a directory of bundles.
//
// Nothing is loaded here. A control plane may know of more tenants than it
// serves in a day, and reading every bundle at start-up would make boot time
// proportional to the customer list.
func NewRegistry(root string, reload time.Duration, log *slog.Logger) *Registry {
	if log == nil {
		log = slog.Default()
	}

	return &Registry{
		root:    root,
		log:     log,
		reload:  reload,
		loaders: map[policy.TenantID]*Loader{},
		cancels: map[policy.TenantID]context.CancelFunc{},
	}
}

// For returns a tenant's source, loading it the first time it is asked for.
//
// A bundle that does not compile is an error rather than an empty source. An
// empty bundle denies everything, which sounds like the safe direction and is
// not: it locks a customer out of their own repositories because a file has a
// typo in it, with nothing saying so.
func (r *Registry) For(ctx context.Context, tenant policy.TenantID) (policy.Source, error) {
	if tenant == "" {
		return nil, fmt.Errorf("policyloader: no tenant")
	}

	r.mu.Lock()

	if loader, ok := r.loaders[tenant]; ok {
		r.mu.Unlock()
		return loader, nil
	}

	r.mu.Unlock()

	dir, err := r.dirFor(tenant)
	if err != nil {
		return nil, err
	}

	// Loaded outside the lock: reading and compiling a bundle can take long
	// enough that holding it would stall every other tenant's requests behind
	// one slow directory.
	loader, err := New(dir, r.log.With("tenant", string(tenant)))
	if err != nil {
		return nil, fmt.Errorf("policyloader: tenant %s: %w", tenant, err)
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	// Another request may have loaded it while this one was compiling. Keeping
	// the first is what makes the pointer a tenant receives stable.
	if existing, ok := r.loaders[tenant]; ok {
		return existing, nil
	}

	r.loaders[tenant] = loader

	if r.reload > 0 {
		watchCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		r.cancels[tenant] = cancel

		go func() {
			if err := loader.Watch(watchCtx, r.reload); err != nil && watchCtx.Err() == nil {
				r.log.Error("policy watcher stopped", "tenant", tenant, "error", err)
			}
		}()
	}

	return loader, nil
}

// dirFor locates a tenant's bundle.
func (r *Registry) dirFor(tenant policy.TenantID) (string, error) {
	scoped := filepath.Join(r.root, string(tenant))

	if _, err := os.Stat(scoped); err == nil {
		return scoped, nil
	}

	// The compatibility case, and only for the default tenant: an existing
	// deployment points policy.dir straight at a bundle. Extending the fallback
	// to every tenant would mean a missing bundle silently served somebody
	// else's rules, which is the one mistake this whole layer exists to make
	// impossible.
	if tenant == policy.DefaultTenant {
		if _, err := os.Stat(r.root); err == nil {
			return r.root, nil
		}
	}

	return "", fmt.Errorf("policyloader: no bundle for tenant %s at %s", tenant, scoped)
}

// Close stops every watcher.
func (r *Registry) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()

	for tenant, cancel := range r.cancels {
		cancel()
		delete(r.cancels, tenant)
	}
}

// Tenants reports which bundles are resident, for diagnostics.
func (r *Registry) Tenants() []policy.TenantID {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]policy.TenantID, 0, len(r.loaders))
	for tenant := range r.loaders {
		out = append(out, tenant)
	}

	return out
}

var _ policy.Sources = (*Registry)(nil)
