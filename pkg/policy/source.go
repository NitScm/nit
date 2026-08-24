package policy

import (
	"context"
	"errors"
)

// Source provides the bundle currently in force.
//
// The interface is here, in the engine's own package, rather than beside the
// loader that watches a directory: a directory is one way to obtain a bundle,
// not the definition of one. An implementation may read from object storage,
// generate rules from another system, or — the case this was extracted for —
// compose a bundle from files with group membership resolved against a company
// directory, so that `subject: {type: group, id: platform}` means what that
// company already means by it.
//
// What an implementation may not do is decide anything. It returns a compiled
// bundle; Evaluate does the rest, and stays the single decision point (D9).
//
// See docs/EXTENSIONS.md.
type Source interface {
	// Current returns the bundle to evaluate against. It is called on the
	// request path, so it must be cheap and must not block: implementations
	// that fetch from elsewhere refresh in the background and serve the last
	// good bundle meanwhile, which is what the directory loader does.
	Current() *Policy
}

// Static is a Source wrapping one immutable bundle.
//
// Useful in tests, in the worker, and anywhere the bundle is decided once at
// start-up rather than watched.
type Static struct{ Policy *Policy }

// Current implements Source.
func (s Static) Current() *Policy { return s.Policy }

// Sources provides the bundle source of each tenant.
//
// The interface exists rather than a tenant argument on Source.Current for one
// reason: `Current` is called on the request path and must be an atomic load,
// while *finding* a tenant's source may read a directory or fetch from
// elsewhere. Separating them keeps the cheap thing cheap, and leaves every
// existing Source — including one written out of tree — working unchanged.
//
// A single-tenant deployment uses OneSource and never notices this exists.
type Sources interface {
	// For returns the source of one tenant's bundle.
	//
	// An unknown tenant is an error, not an empty bundle. A caller that
	// received an empty one would evaluate against no rules, and no rules means
	// deny — which sounds safe and is: it locks a paying customer out of their
	// own repositories because a lookup failed. The error says which tenant, so
	// the failure is diagnosable rather than mysterious.
	For(ctx context.Context, tenant TenantID) (Source, error)
}

// OneSource serves the same bundle to every tenant.
//
// It is what a single-tenant deployment runs, and it is deliberately not a
// degraded mode: one bundle for one tenant is the whole truth there, and the
// registry that keeps N of them would be an empty abstraction.
type OneSource struct{ Source Source }

// For implements Sources.
func (o OneSource) For(context.Context, TenantID) (Source, error) {
	if o.Source == nil {
		return nil, errors.New("policy: no source configured")
	}

	return o.Source, nil
}

var _ Sources = OneSource{}
