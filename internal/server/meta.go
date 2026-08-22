package server

import (
	"net/http"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/internal/store"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
)

// WhoAmI describes the authenticated caller.
type WhoAmI struct {
	User   string   `json:"user"`
	Email  string   `json:"email"`
	Groups []string `json:"groups"`

	PolicyVersion string `json:"policy_version"`
}

func (s *Server) handleWhoAmI(w http.ResponseWriter, r *http.Request) error {
	principal := auth.PrincipalFrom(r.Context())

	groups := make([]string, 0, len(principal.Subject.Groups))
	for _, g := range principal.Subject.Groups {
		groups = append(groups, string(g))
	}

	writeJSON(w, http.StatusOK, WhoAmI{
		User:          string(principal.User.PolicyUserID),
		Email:         principal.User.Email,
		Groups:        groups,
		PolicyVersion: principal.PolicyVersion,
	})

	return nil
}

// RepositoryView is a repository as a client sees it.
type RepositoryView struct {
	ID            string `json:"id"`
	DefaultBranch string `json:"default_branch"`
	Forge         string `json:"forge"`
}

// handleRepositories lists the repositories the caller can see.
//
// "Can see" means the policy grants them read on at least something. Listing
// every repository under nit control would tell a contractor the names of every
// project in the company, which is exactly the kind of leak the product exists
// to prevent.
func (s *Server) handleRepositories(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	principal := auth.PrincipalFrom(ctx)

	repos, err := s.deps.Store.Repositories().List(ctx, s.cfg.Tenant)
	if err != nil {
		return err
	}

	current := s.deps.Policy.Current()

	views := make([]RepositoryView, 0, len(repos))

	for _, repo := range repos {
		if !readableSomewhere(current, principal.Subject, repo.PolicyRepoID) {
			continue
		}

		views = append(views, RepositoryView{
			ID:            string(repo.PolicyRepoID),
			DefaultBranch: repo.DefaultBranch,
			Forge:         repo.Forge,
		})
	}

	writeJSON(w, http.StatusOK, views)

	return nil
}

// readableSomewhere reports whether the subject may read anything in a
// repository.
//
// It asks the engine about each rule's own patterns rather than inventing a
// probe path: a repository whose only grant is on "src/server/" would look
// unreadable to a probe of "/" or of "README.md", and would vanish from a
// developer's list for no reason they could diagnose.
func readableSomewhere(p *policy.Policy, subject policy.Subject, repo policy.RepoID) bool {
	for _, rule := range p.Rules(repo) {
		if rule.Effect != policy.EffectAllow || !rule.HasAction(policy.ActionRead) {
			continue
		}
		if !rule.MatchesSubject(subject) {
			continue
		}

		for _, pattern := range rule.Paths {
			probe := pattern.String()

			// A subtree pattern matches its own directory entry, and "**"
			// matches anything; either way the rule's own pattern is the
			// cheapest witness that something is readable.
			if d := p.Evaluate(policy.Request{
				Repo:    repo,
				Subject: subject,
				Path:    probe,
				Action:  policy.ActionRead,
			}); d.Allowed {
				return true
			}
		}
	}

	return false
}

// WorkspaceView is a workspace as a client sees it.
type WorkspaceView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// CreateWorkspaceRequest registers a checkout.
type CreateWorkspaceRequest struct {
	// Label is free text shown to operators, typically a machine name.
	Label string `json:"label"`
}

// handleCreateWorkspace registers a new checkout for the caller.
//
// A workspace is created explicitly rather than implied by the first push,
// because it is the key of a sync point: auto-creating one on an unrecognized
// id would let a typo silently start a second, empty projection instead of
// failing.
func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	principal := auth.PrincipalFrom(ctx)

	var req CreateWorkspaceRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}

	workspace, err := s.deps.Store.Workspaces().Create(ctx, &store.Workspace{
		TenantID:  s.cfg.Tenant,
		UserID:    principal.User.ID,
		Label:     req.Label,
		CreatedAt: s.deps.Now(),
	})
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusCreated, WorkspaceView{
		ID:    string(workspace.ID),
		Label: workspace.Label,
	})

	return nil
}

func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	principal := auth.PrincipalFrom(ctx)

	workspaces, err := s.deps.Store.Workspaces().ListByUser(ctx, principal.User.ID)
	if err != nil {
		return err
	}

	views := make([]WorkspaceView, 0, len(workspaces))
	for _, workspace := range workspaces {
		views = append(views, WorkspaceView{ID: string(workspace.ID), Label: workspace.Label})
	}

	writeJSON(w, http.StatusOK, views)

	return nil
}

// Health is the liveness response.
type Health struct {
	Status          string `json:"status"`
	ProtocolVersion string `json:"protocol_version"`
	PolicyVersion   string `json:"policy_version"`
}

// handleHealthz is unauthenticated: a load balancer has no token.
//
// It reports the policy version, which is what makes a rolling deploy
// diagnosable — two replicas serving different bundles is otherwise invisible.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, Health{
		Status:          "ok",
		ProtocolVersion: protocol.Version,
		PolicyVersion:   s.deps.Policy.Current().Version(),
	})
}
