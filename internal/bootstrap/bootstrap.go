// Package bootstrap wires a running process from configuration, and
// reconciles the policy bundle into the database.
//
// It exists so that nitd, nit-worker and nitctl start from exactly the same
// configuration and the same reconciliation logic. Three processes that read
// their settings slightly differently is a class of incident that only shows up
// in production.
//
// Configuration is assembled in three layers — built-in defaults, a
// configuration file, then the environment — in config.go and file.go.
package bootstrap

import (
	"context"
	"fmt"
	"time"

	"github.com/NitScm/nit/internal/store"
	"github.com/NitScm/nit/pkg/policy"
)

// ReconcilePolicy makes the database reflect the policy bundle.
//
// Users and repositories get rows so that tasks, sessions and audit records
// have something to reference. Nothing is ever deleted: a user removed from the
// bundle still owns history, and dropping it because a line was deleted from a
// YAML file would destroy exactly the record an audit needs.
//
// Removal from the bundle is not a no-op, though: the user loses every grant,
// and authentication fails with "not in the policy bundle". Revocation is
// immediate, only the history survives.
func ReconcilePolicy(ctx context.Context, st store.Store, p *policy.Policy, tenant policy.TenantID) error {
	repos := make([]*store.Repository, 0, len(p.Repositories()))

	for _, repo := range p.Repositories() {
		repos = append(repos, &store.Repository{
			TenantID:      tenant,
			PolicyRepoID:  repo.ID,
			Remote:        repo.Remote,
			Forge:         repo.Forge,
			DefaultBranch: defaultBranch(repo.DefaultBranch),
			PolicyVersion: p.Version(),
			UpdatedAt:     time.Now().UTC(),
		})
	}

	if err := st.Repositories().Reconcile(ctx, tenant, repos); err != nil {
		return fmt.Errorf("bootstrap: reconcile repositories: %w", err)
	}

	return nil
}

// ReconcileUser creates or updates the row backing one bundle user.
//
// Users are reconciled on demand rather than in bulk: the bundle is the source
// of truth for authorization, and a row is only needed once someone issues a
// token or performs an action.
func ReconcileUser(ctx context.Context, st store.Store, p *policy.Policy, tenant policy.TenantID, id policy.UserID) (*store.User, error) {
	declared, ok := p.User(id)
	if !ok {
		return nil, fmt.Errorf("bootstrap: user %q is not in the policy bundle", id)
	}

	return st.Users().Upsert(ctx, &store.User{
		TenantID:     tenant,
		PolicyUserID: declared.ID,
		Email:        declared.Email,
		Disabled:     declared.Disabled,
		UpdatedAt:    time.Now().UTC(),
	})
}

func defaultBranch(branch string) string {
	if branch == "" {
		return "main"
	}
	return branch
}
