package worker_test

import (
	"github.com/NitScm/nit/internal/synctoken"
	"github.com/NitScm/nit/pkg/policy"
)

// newSigner builds the default tenant's signer, which is what a single-tenant
// deployment runs with.
func newSigner(key []byte) (*synctoken.Signer, error) {
	root, err := synctoken.NewRoot(key)
	if err != nil {
		return nil, err
	}

	return root.For(policy.DefaultTenant)
}
