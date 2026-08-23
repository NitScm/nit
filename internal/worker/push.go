package worker

import (
	"context"
	"encoding/json"
	"errors"
	"io"

	"github.com/NitScm/nit/internal/blob"
	"github.com/NitScm/nit/internal/compress"
	"github.com/NitScm/nit/internal/taskspec"
	"github.com/NitScm/nit/pkg/gitx"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// handlePush applies an already-authorized patch and publishes it.
//
// No authorization happens here. The control plane decided, refused what had to
// be refused, and stored the enforced patch; re-deriving the decision in the
// worker would only create a way for the two to disagree.
func (w *Worker) handlePush(ctx context.Context, task *store.Task) ([]byte, error) {
	spec, err := loadSpec[taskspec.Push](task)
	if err != nil {
		return nil, err
	}

	patch, err := w.loadEnforcedPatch(ctx, spec)
	if err != nil {
		return nil, err
	}

	remote, err := w.authenticatedRemote(ctx, spec.Forge, spec.Remote)
	if err != nil {
		return nil, err
	}

	repo, cleanup, err := w.clone(ctx, remote, spec.Branch)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	tipBefore, err := repo.ResolveRef(ctx, "HEAD")
	if err != nil {
		return nil, err
	}

	// Work from the base the author actually had. Applying to the current tip
	// instead would silently resolve, or silently mangle, changes that landed
	// in between.
	if err := repo.Checkout(ctx, spec.BaseCommit); err != nil {
		return nil, permanent(protocol.CodeConflict,
			"the base commit %s is no longer present upstream; pull and push again", short(spec.BaseCommit))
	}

	if err := repo.Apply(ctx, patch, gitx.ApplyOptions{ThreeWay: true}); err != nil {
		if errors.Is(err, gitx.ErrConflict) {
			return nil, permanent(protocol.CodeConflict,
				"the patch no longer applies to %s; pull, resolve, and push again", spec.Branch)
		}
		return nil, err
	}

	// The message carries the trailers that make this commit traceable from the
	// forge alone, and no longer carries anything the author wrote that
	// impersonates one.
	commit, err := repo.CommitAll(ctx, commitMessage(spec, task.ID), gitx.Author{
		Name:  spec.AuthorName,
		Email: spec.AuthorEmail,
		When:  w.deps.Now(),
	})
	if err != nil {
		return nil, err
	}

	// Replay onto whatever landed while this task was queued. A conflict here
	// is the author's to resolve: retrying would conflict identically.
	rebased := false

	if tipBefore != spec.BaseCommit {
		if err := repo.Rebase(ctx, tipBefore, gitx.Author{
			Name:  spec.AuthorName,
			Email: spec.AuthorEmail,
		}); err != nil {
			if errors.Is(err, gitx.ErrConflict) {
				return nil, permanent(protocol.CodeConflict,
					"%s moved while your push was queued and your change no longer applies; pull, resolve, and push again",
					spec.Branch)
			}
			return nil, err
		}

		rebased = true

		if commit, err = repo.ResolveRef(ctx, "HEAD"); err != nil {
			return nil, err
		}
	}

	// --force-with-lease is the real atomicity guarantee. The queue serializes
	// nit's own work; only the forge can arbitrate against a change that did
	// not come through nit at all.
	if err := repo.Push(ctx, "origin", spec.Branch, gitx.PushOptions{
		ExpectedRemoteCommit: tipBefore,
	}); err != nil {
		return nil, permanent(protocol.CodeConflict,
			"%s moved on the forge while this push was running; pull and push again", spec.Branch)
	}

	result := protocol.PushResult{
		UpstreamCommit: commit,
		Report:         protocol.PushReport{PolicyVersion: spec.PolicyVersion},
	}

	// The workspace only remains a faithful projection of upstream when this
	// push was the sole change and nothing was dropped from it. If the branch
	// moved, or the patch was stripped, the developer's tree no longer matches
	// what landed and the only honest answer is to make them pull.
	if !rebased && spec.DroppedFiles == 0 {
		token, err := w.advanceSyncPoint(ctx, spec, commit)
		if err != nil {
			return nil, err
		}
		result.NextSync = token
	}

	w.audit.Record(ctx, spec.Repository, &store.AuditRecord{
		TenantID:      w.cfg.Tenant,
		OccurredAt:    w.deps.Now(),
		ActorUserID:   spec.UserID,
		ActorLabel:    string(spec.PolicyUserID),
		Action:        "push.applied",
		RepositoryID:  task.RepositoryID,
		Branch:        spec.Branch,
		PolicyVersion: spec.PolicyVersion,
		RequestID:     spec.RequestID,
		TaskID:        task.ID,
		Detail:        detail(map[string]string{"upstream_commit": commit, "base": spec.BaseCommit}),
	})

	return json.Marshal(result)
}

// loadEnforcedPatch fetches the patch the control plane authorized.
func (w *Worker) loadEnforcedPatch(ctx context.Context, spec taskspec.Push) ([]byte, error) {
	reader, err := w.deps.Blobs.Get(ctx, spec.PatchDigest)
	if errors.Is(err, blob.ErrNotFound) {
		return nil, permanent("missing_patch",
			"the authorized patch %s is no longer in the blob store", short(spec.PatchDigest))
	}
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	raw, err := io.ReadAll(io.LimitReader(reader, w.cfg.MaxPatchBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > w.cfg.MaxPatchBytes {
		return nil, permanent(protocol.CodePatchTooLarge, "the stored patch exceeds the size limit")
	}

	return compress.Decompress(raw, spec.PatchEncoding, w.cfg.MaxPatchBytes)
}

// advanceSyncPoint moves the workspace's recorded projection forward and mints
// the token for it.
//
// CompareAndSet, not Put: if something else moved this workspace's sync point
// while the task ran, the client is no longer where this task assumed, and
// overwriting the record would leave the server describing a state the client
// does not have.
func (w *Worker) advanceSyncPoint(ctx context.Context, spec taskspec.Push, commit string) (protocol.SyncToken, error) {
	repo, err := w.deps.Store.Repositories().ByPolicyID(ctx, w.cfg.Tenant, spec.Repository)
	if err != nil {
		return "", err
	}

	next := &store.SyncPoint{
		TenantID:       w.cfg.Tenant,
		WorkspaceID:    spec.WorkspaceID,
		RepositoryID:   repo.ID,
		Branch:         spec.Branch,
		UpstreamCommit: commit,
		PolicyVersion:  spec.PolicyVersion,
		UpdatedAt:      w.deps.Now(),
	}

	if err := w.deps.Store.SyncPoints().CompareAndSet(ctx, next, spec.BaseCommit); err != nil {
		// The push has already landed on the forge. Failing the task now would
		// mean retrying a push that succeeded, so a bookkeeping problem must
		// never be fatal here. Returning no token makes the client pull, which
		// reconciles whatever the disagreement was.
		if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrNotFound) {
			w.deps.Log.WarnContext(ctx, "sync point could not be advanced; the client must pull",
				"workspace", spec.WorkspaceID, "branch", spec.Branch, "reason", err)
			return "", nil
		}
		return "", err
	}

	return w.deps.SyncTokens.Sign(syncPayload(spec.WorkspaceID, repo.ID, spec.Branch, commit, spec.PolicyVersion, w.deps.Now().Unix()))
}

func short(s string) string {
	if len(s) > 12 {
		return s[:12]
	}
	return s
}

func detail(fields map[string]string) []byte {
	encoded, err := json.Marshal(fields)
	if err != nil {
		return nil
	}
	return encoded
}

// audit appends a record, best effort.
//
// A worker that failed to write its audit line has still performed the work;
// failing the task would re-run a push that already landed. The failure is
// loud and the operator decides.
