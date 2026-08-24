package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/internal/queue"
	"github.com/NitScm/nit/internal/taskspec"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// handlePull queues the production of a filtered diff.
//
// Unlike a push, nothing is authorized here. A pull cannot be refused — the
// developer simply does not receive what they may not read — and what is
// readable has to be decided against the bundle in force when the diff is
// produced, not when the request was made. The filtering therefore happens in
// the worker, next to the diff.
//
// A pull also takes no partition key: it is read-only, so any number of them
// run in parallel with each other and with a push on the same branch.
func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	principal := auth.PrincipalFrom(ctx)

	var req protocol.PullRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if err := checkProtocolVersion(req.ProtocolVersion); err != nil {
		return err
	}
	if req.RequestID == "" {
		return fail(http.StatusBadRequest, "bad_request", "request_id is required")
	}

	if existing, err := s.deps.Store.Tasks().ByRequestID(ctx, tenantOf(ctx), req.RequestID); err == nil {
		writeJSON(w, http.StatusAccepted, protocol.PullResponse{TaskID: string(existing.ID)})
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	repo, err := s.resolveRepository(ctx, req.Repository)
	if err != nil {
		return err
	}

	workspace, err := s.resolveWorkspace(ctx, principal, req.Workspace)
	if err != nil {
		return err
	}

	branch := req.Branch
	if branch == "" {
		branch = repo.DefaultBranch
	}

	fromCommit, err := s.resolvePullBase(ctx, req.Sync, workspace.ID, repo.ID, branch)
	if err != nil {
		return err
	}

	current := s.deps.Policy.Current()

	spec := taskspec.Pull{
		RequestID:     req.RequestID,
		Repository:    repo.PolicyRepoID,
		Remote:        repo.Remote,
		Forge:         repo.Forge,
		Branch:        branch,
		FromCommit:    fromCommit,
		UserID:        principal.User.ID,
		WorkspaceID:   workspace.ID,
		PolicyUserID:  principal.User.PolicyUserID,
		PolicyVersion: current.Version(),
	}

	payload, err := json.Marshal(spec)
	if err != nil {
		return err
	}

	task, _, err := s.deps.Queue.Submit(ctx, &store.Task{
		TenantID:     tenantOf(ctx),
		RequestID:    req.RequestID,
		Kind:         protocol.TaskPull,
		UserID:       principal.User.ID,
		WorkspaceID:  workspace.ID,
		RepositoryID: repo.ID,
		Branch:       branch,
		PartitionKey: queue.PartitionKey(protocol.TaskPull, string(repo.PolicyRepoID), branch),
		Payload:      payload,
		CreatedAt:    s.deps.Now(),
	})
	if err != nil {
		return err
	}

	s.audit.Record(ctx, repo.PolicyRepoID, &store.AuditRecord{
		TenantID:      tenantOf(ctx),
		OccurredAt:    s.deps.Now(),
		ActorUserID:   principal.User.ID,
		ActorLabel:    string(principal.User.PolicyUserID),
		Action:        "pull.requested",
		RepositoryID:  repo.ID,
		Branch:        branch,
		PolicyVersion: current.Version(),
		RequestID:     req.RequestID,
		TaskID:        task.ID,
	})

	writeJSON(w, http.StatusAccepted, protocol.PullResponse{TaskID: string(task.ID)})

	return nil
}

// resolvePullBase returns the commit a pull should diff from.
//
// The base comes from the client's token, not from the server's stored sync
// point, and the difference matters.
//
// The stored point records the furthest projection nit has produced for a
// workspace. The token records what the client actually holds. They diverge
// whenever a pull is delivered and the client fails to apply it — a crash, a
// disk full, an interrupted command. Diffing from the stored point would then
// hand the client a patch that assumes changes it never received, and refusing
// the request as stale would leave it with no way to catch up at all: every
// later pull would be refused for the same reason. Deriving the diff from where
// the client says it is makes that case self-correcting.
//
// The token is signed, so its claim is not a client's invention: the server
// minted it for this workspace, on this branch, at that commit. Verifying it is
// what makes trusting the claim safe.
//
// An empty token is legitimate here, unlike on a push: a workspace that has
// never synchronized needs a full filtered snapshot, which is what "nit clone"
// asks for.
func (s *Server) resolvePullBase(_ context.Context, token protocol.SyncToken, workspaceID, repoID store.ID, branch string) (string, error) {
	if token == "" {
		return "", nil
	}

	payload, err := s.deps.SyncTokens.Verify(token)
	if err != nil {
		return "", fail(http.StatusBadRequest, "bad_request", "invalid sync token")
	}
	if !payload.Matches(workspaceID, repoID, branch) {
		return "", fail(http.StatusBadRequest, "bad_request",
			"the sync token was issued for another workspace, repository or branch")
	}

	return payload.UpstreamCommit, nil
}
