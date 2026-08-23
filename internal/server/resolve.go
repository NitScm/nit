package server

import (
	"context"
	"errors"
	"net/http"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/internal/synctoken"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// resolveRepository maps a policy repository id to its stored record.
func (s *Server) resolveRepository(ctx context.Context, name string) (*store.Repository, error) {
	if name == "" {
		return nil, fail(http.StatusBadRequest, "bad_request", "repository is required")
	}

	repo, err := s.deps.Store.Repositories().ByPolicyID(ctx, s.cfg.Tenant, policy.RepoID(name))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fail(http.StatusNotFound, protocol.CodeUnknownRepository,
			"repository %q is not under nit control", name)
	}
	if err != nil {
		return nil, err
	}

	return repo, nil
}

// resolveWorkspace loads a workspace and checks it belongs to the caller.
//
// The ownership check is the point. A workspace id names a sync point, and a
// sync point decides which upstream commit a patch is applied to; accepting one
// belonging to somebody else would let a caller push onto another developer's
// base. The response is 404 rather than 403 so it does not confirm that an id
// exists.
func (s *Server) resolveWorkspace(ctx context.Context, principal *auth.Principal, id protocol.WorkspaceID) (*store.Workspace, error) {
	if id == "" {
		return nil, fail(http.StatusBadRequest, "bad_request",
			"workspace is required; create one with POST /v1/workspaces")
	}

	workspace, err := s.deps.Store.Workspaces().ByID(ctx, store.ID(id))
	if errors.Is(err, store.ErrNotFound) {
		return nil, fail(http.StatusNotFound, "unknown_workspace", "unknown workspace %q", id)
	}
	if err != nil {
		return nil, err
	}

	if workspace.UserID != principal.User.ID {
		return nil, fail(http.StatusNotFound, "unknown_workspace", "unknown workspace %q", id)
	}

	return workspace, nil
}

// resolveBaseCommit verifies a sync token and returns the upstream commit it
// names.
//
// Three checks, each closing a different hole:
//
//   - the signature proves the server minted the token, so a client cannot
//     name a base of its own choosing;
//   - Matches proves it was minted for *this* workspace, repository and branch,
//     so a token issued elsewhere cannot be replayed here;
//   - the comparison against the stored sync point proves it is still current,
//     so a patch computed against a base the workspace has since moved off is
//     refused rather than applied somewhere it does not belong.
func (s *Server) resolveBaseCommit(ctx context.Context, token protocol.SyncToken, workspaceID, repoID store.ID, branch string) (string, error) {
	stored, err := s.deps.Store.SyncPoints().Get(ctx, workspaceID, repoID, branch)

	if errors.Is(err, store.ErrNotFound) {
		return "", fail(http.StatusConflict, protocol.CodeUnknownSyncPoint,
			"this workspace has no sync point for %s; run: nit pull", branch)
	}
	if err != nil {
		return "", err
	}

	if token == "" {
		return "", fail(http.StatusConflict, protocol.CodeStaleSyncPoint,
			"no sync token supplied; run: nit pull")
	}

	payload, err := s.deps.SyncTokens.Verify(token)
	if err != nil {
		return "", fail(http.StatusBadRequest, "bad_request", "invalid sync token")
	}

	if !payload.Matches(workspaceID, repoID, branch) {
		return "", fail(http.StatusBadRequest, "bad_request",
			"the sync token was issued for another workspace, repository or branch")
	}

	if payload.UpstreamCommit != stored.UpstreamCommit {
		return "", fail(http.StatusConflict, protocol.CodeStaleSyncPoint,
			"your workspace is behind; run: nit pull")
	}

	return stored.UpstreamCommit, nil
}

// mintSyncToken issues a token for a sync point.
func (s *Server) mintSyncToken(workspaceID, repoID store.ID, branch, commit, policyVersion string) (protocol.SyncToken, error) {
	return s.deps.SyncTokens.Sign(synctoken.Payload{
		Workspace:      workspaceID,
		Repository:     repoID,
		Branch:         branch,
		UpstreamCommit: commit,
		PolicyVersion:  policyVersion,
		IssuedAt:       s.deps.Now().Unix(),
	})
}
