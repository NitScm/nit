package server

import (
	"context"
	"net/http"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/pkg/policy"
)

// tenantOf returns whose data this request may touch.
//
// It comes from the authenticated principal — that is, from the token — and not
// from the process's configuration. A tenant fixed at start-up means a process
// serves one customer, which is right for a self-hosted deployment and the
// thing a hosted control plane cannot do.
//
// # Why it falls back to the default rather than failing
//
// Every route that reaches this is authenticated, so a nil principal is a
// routing mistake rather than an unauthenticated caller. Returning the default
// tenant is what a single-tenant deployment expects and what every one of them
// runs with today.
//
// **That fallback stops being safe the day a deployment has a second tenant**,
// because it turns "the principal is missing" into "read the first tenant's
// data". It is the reason gap 1 in saas-thinking/03 is not finished by this
// function: the remaining work is to make an absent principal impossible to
// reach here, and PostgreSQL row-level security is the second layer that makes
// forgetting a failed query rather than a cross-tenant read.
func tenantOf(ctx context.Context) policy.TenantID {
	if principal := auth.PrincipalFrom(ctx); principal != nil && principal.Tenant != "" {
		return principal.Tenant
	}

	return policy.DefaultTenant
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
