package server

import (
	"context"
	"net/http"

	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// tenantOf returns whose data this request may touch.
//
// It comes from the authenticated principal — that is, from the token — and not
// from the process's configuration. A tenant fixed at start-up means a process
// serves one customer, which is right for a self-hosted deployment and the
// thing a hosted control plane cannot do.
//
// # One source of truth, and no fallback
//
// It reads the tenant the *store* reads — the one `serve` put in the context
// beside the principal — rather than deriving it a second time from the
// principal itself. The two are set together in one place and cannot be set
// apart, so asking twice bought nothing and cost the possibility of
// disagreeing.
//
// It used to fall back to the default tenant when no principal was present, on
// the reasoning that every route reaching here is authenticated so an absent
// principal is a routing mistake. That was true and the fallback was still
// wrong: it turned "the principal is missing" into "read the first tenant's
// data", which is correct-looking in the single-tenant deployment everybody
// runs today and a cross-tenant read on the first day there are two.
//
// Now an absent principal yields the empty tenant, and the empty tenant matches
// nothing: PostgreSQL stamps it on the connection and row-level security
// returns no rows, MySQL puts it in the WHERE clause and it matches none. A
// routing mistake becomes an empty result somebody reports rather than a
// disclosure nobody notices.
//
// Removing it also closed a way for the two layers to disagree. This function
// could answer "default" while the connection was stamped with the empty
// string, producing a query asking for one tenant's rows over a connection
// permitted to read none — an empty result with a confusing cause.
func tenantOf(ctx context.Context) policy.TenantID {
	return store.TenantFrom(ctx)
}

// policyFor returns the bundle in force for this request's tenant.
//
// One lookup per request rather than one bundle per process. A self-hosted
// deployment hands back the same bundle every time — policy.OneSource — and a
// hosted control plane hands back that customer's.
//
// An error here is a 503 rather than a denial. A tenant whose bundle cannot be
// resolved has no rules, and no rules means deny everything, which sounds like
// the safe direction until it means a paying customer locked out of their own
// repositories because a file has a typo in it. Saying "the policy is
// unavailable" is both truer and actionable.
func (s *Server) policyFor(ctx context.Context) (*policy.Policy, error) {
	source, err := s.deps.Policy.For(ctx, tenantOf(ctx))
	if err != nil {
		return nil, fail(http.StatusServiceUnavailable, "policy_unavailable",
			"the policy bundle for this tenant could not be loaded")
	}

	return source.Current(), nil
}
