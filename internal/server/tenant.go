package server

import (
	"context"

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
