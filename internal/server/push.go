package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/internal/blob"
	"github.com/NitScm/nit/internal/compress"
	"github.com/NitScm/nit/internal/queue"
	"github.com/NitScm/nit/internal/taskspec"
	"github.com/NitScm/nit/pkg/enforce"
	"github.com/NitScm/nit/pkg/patch"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// handlePush accepts a change, authorizes it, and queues the work.
//
// The order of operations is the design, not an accident:
//
//  1. deduplicate, so a retry cannot become a second upstream commit;
//  2. verify the sync point, so the patch is applied to the base its author
//     actually had;
//  3. authorize, before anything is queued, so a refused push costs no clone;
//  4. store the *enforced* patch, so the worker cannot apply the client's
//     version by accident;
//  5. enqueue.
func (s *Server) handlePush(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	principal := auth.PrincipalFrom(ctx)

	var req protocol.PushRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if err := checkProtocolVersion(req.ProtocolVersion); err != nil {
		return err
	}
	if req.RequestID == "" {
		return fail(http.StatusBadRequest, "bad_request", "request_id is required")
	}
	if req.Branch == "" {
		return fail(http.StatusBadRequest, "bad_request", "branch is required")
	}
	// A dry run creates no commit, so it needs no message. Requiring one would
	// make "nit push --check" impossible to run before deciding what to write.
	if req.Message == "" && !req.DryRun {
		return fail(http.StatusBadRequest, "bad_request", "message is required")
	}

	// A retried submission returns its original task. Doing this first means a
	// client that lost the response never pays for the work twice.
	if existing, err := s.deps.Store.Tasks().ByRequestID(ctx, s.cfg.Tenant, req.RequestID); err == nil {
		return s.respondExistingPush(ctx, w, existing)
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

	baseCommit, err := s.resolveBaseCommit(ctx, req.BaseSync, workspace.ID, repo.ID, req.Branch)
	if err != nil {
		return err
	}

	raw, err := s.loadPatch(ctx, req.Patch)
	if err != nil {
		return err
	}

	set, err := patch.Parse(raw)
	if errors.Is(err, patch.ErrCombinedDiff) {
		return fail(http.StatusBadRequest, "bad_request",
			"combined (merge) diffs are not supported; push a flat diff")
	}
	if errors.Is(err, patch.ErrEmpty) {
		return fail(http.StatusBadRequest, "bad_request", "the patch contains no change")
	}
	if err != nil {
		return fail(http.StatusBadRequest, "bad_request", "the patch could not be parsed: %v", err)
	}

	mode := enforce.ModeReject
	if req.Mode == protocol.PushModeStrip {
		mode = enforce.ModeStrip
	}

	current := s.deps.Policy.Current()

	result, err := enforce.Push(set, enforce.Options{
		Engine:  current,
		Repo:    repo.PolicyRepoID,
		Ref:     "refs/heads/" + req.Branch,
		Subject: principal.Subject,
		Guards:  s.cfg.Guards,
	}, mode)
	if err != nil {
		return err
	}

	report := pushReport(result)

	s.auditPush(ctx, principal, repo, req, result)

	if result.Rejected {
		return &apiError{
			status: http.StatusForbidden,
			body: &protocol.Error{
				Code:    protocol.CodeUnauthorizedPaths,
				Message: "the patch touches paths you may not change",
				Denials: report.Denials,
			},
		}
	}

	if req.DryRun {
		writeJSON(w, http.StatusOK, protocol.PushResponse{Accepted: false, Report: report})
		return nil
	}

	if len(result.Patch) == 0 {
		return fail(http.StatusBadRequest, "bad_request",
			"nothing remains of the patch after filtering; no change would be applied")
	}

	// Store the enforced patch, not the uploaded one. In strip mode they
	// differ, and a worker applying the client's version would undo the entire
	// authorization pass. Content addressing makes this free when they are the
	// same bytes.
	enforced, err := s.storePatch(ctx, result.Patch, store.ArtifactPushPatch, nil)
	if err != nil {
		return err
	}

	spec := taskspec.Push{
		RequestID:     req.RequestID,
		Repository:    repo.PolicyRepoID,
		Remote:        repo.Remote,
		Forge:         repo.Forge,
		Branch:        req.Branch,
		BaseCommit:    baseCommit,
		PatchDigest:   enforced.Digest,
		PatchEncoding: protocol.EncodingNone,
		Message:       req.Message,
		DroppedFiles:  len(result.Dropped),

		// From the authenticated identity, never from the request: a commit
		// author field is free text.
		AuthorName:  displayName(principal),
		AuthorEmail: principal.User.Email,

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
		TenantID:     s.cfg.Tenant,
		RequestID:    req.RequestID,
		Kind:         protocol.TaskPush,
		UserID:       principal.User.ID,
		WorkspaceID:  workspace.ID,
		RepositoryID: repo.ID,
		Branch:       req.Branch,
		PartitionKey: queue.PartitionKey(protocol.TaskPush, string(repo.PolicyRepoID), req.Branch),
		Payload:      payload,
		CreatedAt:    s.deps.Now(),
	})
	if err != nil {
		return err
	}

	position, err := s.deps.Queue.Position(ctx, task.ID)
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusAccepted, protocol.PushResponse{
		TaskID:        string(task.ID),
		Accepted:      true,
		Report:        report,
		QueuePosition: position,
	})

	return nil
}

// respondExistingPush answers a retried submission with its original task.
func (s *Server) respondExistingPush(ctx context.Context, w http.ResponseWriter, task *store.Task) error {
	position, err := s.deps.Queue.Position(ctx, task.ID)
	if err != nil {
		return err
	}

	writeJSON(w, http.StatusAccepted, protocol.PushResponse{
		TaskID:        string(task.ID),
		Accepted:      true,
		QueuePosition: position,
	})

	return nil
}

// loadPatch fetches an uploaded blob and decompresses it.
func (s *Server) loadPatch(ctx context.Context, descriptor protocol.Blob) ([]byte, error) {
	if descriptor.Digest == "" {
		return nil, fail(http.StatusBadRequest, "bad_request", "patch.digest is required")
	}
	if descriptor.Size > s.cfg.MaxPatchBytes {
		return nil, fail(http.StatusRequestEntityTooLarge, protocol.CodePatchTooLarge,
			"the patch is %d bytes, the limit is %d", descriptor.Size, s.cfg.MaxPatchBytes)
	}

	reader, err := s.deps.Blobs.Get(ctx, descriptor.Digest)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, fail(http.StatusBadRequest, "unknown_blob",
			"patch %s was not uploaded", descriptor.Digest)
	}
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	compressed, err := io.ReadAll(io.LimitReader(reader, s.cfg.MaxPatchBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(compressed)) > s.cfg.MaxPatchBytes {
		return nil, fail(http.StatusRequestEntityTooLarge, protocol.CodePatchTooLarge,
			"the patch exceeds the %d byte limit", s.cfg.MaxPatchBytes)
	}

	raw, err := compress.Decompress(compressed, descriptor.Encoding, s.cfg.MaxPatchBytes)
	if errors.Is(err, compress.ErrTooLarge) {
		return nil, fail(http.StatusRequestEntityTooLarge, protocol.CodePatchTooLarge,
			"the patch expands beyond the %d byte limit", s.cfg.MaxPatchBytes)
	}
	if err != nil {
		return nil, fail(http.StatusBadRequest, "bad_request", "the patch could not be decoded: %v", err)
	}

	return raw, nil
}

// storePatch writes bytes to the blob store and records the artifact.
func (s *Server) storePatch(ctx context.Context, raw []byte, kind store.ArtifactKind, expiresAt *time.Time) (protocol.Blob, error) {
	descriptor, err := s.deps.Blobs.Put(ctx, bytes.NewReader(raw), "", s.cfg.MaxPatchBytes)
	if err != nil {
		return protocol.Blob{}, err
	}

	if _, err := s.deps.Store.Artifacts().Create(ctx, &store.Artifact{
		TenantID:         s.cfg.Tenant,
		Digest:           descriptor.Digest,
		Kind:             kind,
		Size:             descriptor.Size,
		UncompressedSize: int64(len(raw)),
		Encoding:         protocol.EncodingNone,
		Locator:          descriptor.Digest,
		CreatedAt:        s.deps.Now(),
		ExpiresAt:        expiresAt,
	}); err != nil {
		return protocol.Blob{}, err
	}

	return protocol.Blob{
		Digest:           descriptor.Digest,
		Size:             descriptor.Size,
		Encoding:         protocol.EncodingNone,
		UncompressedSize: int64(len(raw)),
	}, nil
}

// pushReport flattens an enforcement result for the wire.
func pushReport(result *enforce.Result) protocol.PushReport {
	report := protocol.PushReport{
		PolicyVersion: result.PolicyVersion,
		FilesTotal:    len(result.Verdicts),
		FilesAccepted: len(result.Kept),
		FilesDenied:   len(result.Dropped),
	}

	for _, check := range result.Denials() {
		report.Denials = append(report.Denials, protocol.Denial{
			Path:        check.Path,
			Action:      string(check.Action),
			Guard:       string(check.Guard),
			RuleID:      check.Decision.RuleID,
			Pattern:     check.Decision.Pattern,
			Reason:      string(check.Decision.Reason),
			Description: check.Decision.Description,
		})
	}

	return report
}

// auditPush records the decision.
//
// One row per denied path plus one summary row. Recording every allowed path of
// every push would multiply audit volume by the size of a changeset, for
// information the summary already carries.
func (s *Server) auditPush(ctx context.Context, principal *auth.Principal, repo *store.Repository, req protocol.PushRequest, result *enforce.Result) {
	now := s.deps.Now()

	records := make([]*store.AuditRecord, 0, len(result.Denials())+1)

	action := "push.accepted"
	if result.Rejected {
		action = "push.rejected"
	}

	records = append(records, &store.AuditRecord{
		TenantID:      s.cfg.Tenant,
		OccurredAt:    now,
		ActorUserID:   principal.User.ID,
		ActorLabel:    string(principal.User.PolicyUserID),
		Action:        action,
		RepositoryID:  repo.ID,
		Branch:        req.Branch,
		PolicyVersion: result.PolicyVersion,
		RequestID:     req.RequestID,
	})

	for _, check := range result.Denials() {
		records = append(records, &store.AuditRecord{
			TenantID:      s.cfg.Tenant,
			OccurredAt:    now,
			ActorUserID:   principal.User.ID,
			ActorLabel:    string(principal.User.PolicyUserID),
			Action:        "push.denied_path",
			RepositoryID:  repo.ID,
			Branch:        req.Branch,
			Path:          check.Path,
			Effect:        check.Decision.Effect,
			Reason:        check.Decision.Reason,
			RuleID:        check.Decision.RuleID,
			Guard:         string(check.Guard),
			PolicyVersion: result.PolicyVersion,
			RequestID:     req.RequestID,
		})
	}

	// Best-effort by construction: Record returns nothing, so this call site
	// could not propagate a logging failure into the push even if it wanted to.
	s.audit.Record(ctx, repo.PolicyRepoID, records...)
}

func displayName(p *auth.Principal) string {
	if p.User.DisplayName != "" {
		return p.User.DisplayName
	}
	return string(p.User.PolicyUserID)
}
