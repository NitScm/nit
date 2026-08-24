package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strconv"

	"github.com/NitScm/nit/internal/compress"
	"github.com/NitScm/nit/internal/synctoken"
	"github.com/NitScm/nit/internal/taskspec"
	"github.com/NitScm/nit/pkg/enforce"
	"github.com/NitScm/nit/pkg/patch"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/pullcache"
	"github.com/NitScm/nit/pkg/store"
)

// handlePull produces the filtered diff a developer is allowed to receive.
//
// Unlike a push, the authorization happens here rather than in the control
// plane. What is readable has to be decided against the bundle in force when
// the diff is produced: a rule that changed while the task sat in the queue
// must apply to what is about to be delivered, not to what was current when the
// request arrived.
func (w *Worker) handlePull(ctx context.Context, task *store.Task) ([]byte, error) {
	spec, err := loadSpec[taskspec.Pull](task)
	if err != nil {
		return nil, err
	}

	remote, err := w.authenticatedRemote(ctx, spec.Forge, spec.Remote)
	if err != nil {
		return nil, err
	}

	repo, cleanup, err := w.checkout(ctx, spec.Remote, remote, spec.Branch)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	tip, err := repo.ResolveRef(ctx, "HEAD")
	if err != nil {
		return nil, err
	}

	current, err := w.policyFor(ctx, task.TenantID)
	if err != nil {
		return nil, err
	}

	storeRepo, err := w.deps.Store.Repositories().ByPolicyID(ctx, w.cfg.Tenant, spec.Repository)
	if err != nil {
		return nil, err
	}

	result := protocol.PullResult{
		Report: protocol.PullReport{
			PolicyVersion:  current.Version(),
			UpstreamCommit: tip,
		},
	}

	// Already there: no patch, but the sync point still moves so the client can
	// stop asking.
	if spec.FromCommit == tip {
		token, err := w.recordPullSyncPoint(ctx, spec, storeRepo.ID, tip, current.Version())
		if err != nil {
			return nil, err
		}
		result.NextSync = token

		return json.Marshal(result)
	}

	from := spec.FromCommit

	// A workspace with no sync point needs every file, not a diff. The patch
	// from the empty tree to the tip creates the whole projection, which is
	// exactly what "nit clone" is asking for.
	if from == "" {
		if from, err = repo.EmptyTree(ctx); err != nil {
			return nil, err
		}
	}

	// What a subject receives depends on the subject only through what it may
	// read, so the whole diff-filter-compress-store sequence is shared by
	// everyone with the same rights. On a release day that is the difference
	// between five hundred passes and one per profile.
	//
	// The key is built before any of that work, which is the point: a hit skips
	// the diff too, not only the filtering.
	subject, err := current.Subject(spec.PolicyUserID)
	if err != nil {
		// The account left the bundle while the task was queued. Delivering
		// nothing is the only safe reading, and filterForReader says so with
		// the error a client can act on.
		return nil, permanent("user_not_in_policy",
			"%s is no longer in the policy bundle", spec.PolicyUserID)
	}

	key := pullcache.Key{
		Repository: spec.Remote,
		From:       from,
		To:         tip,
		Profile: current.Profile(storeRepo.PolicyRepoID, "refs/heads/"+spec.Branch,
			policy.ActionRead, subject),
	}

	entry, cached, err := w.pulls.Get(ctx, key)
	if err != nil {
		// A cache that cannot be reached is a miss, never a failed pull: the
		// work it would have saved is work this task can still do.
		w.deps.Log.Warn("pull cache unavailable, recomputing", "error", err)

		cached = false
	}

	if !cached {
		raw, err := repo.Diff(ctx, from, tip)
		if err != nil {
			return nil, err
		}

		filtered, report, err := w.filterForReader(raw, current, spec, storeRepo)
		if err != nil {
			return nil, err
		}

		entry = pullcache.Entry{
			FilesTotal:     report.total,
			FilesDelivered: report.delivered,
			FilesWithheld:  report.withheld,
		}

		if len(filtered) > 0 {
			descriptor, err := w.storePullPatch(ctx, filtered)
			if err != nil {
				return nil, err
			}
			entry.Patch = &descriptor
		}

		if err := w.pulls.Put(ctx, key, entry); err != nil {
			// Same direction: the projection is stored and the client is about
			// to be served. Failing here would throw away finished work.
			w.deps.Log.Warn("could not record the projection for reuse", "error", err)
		}
	}

	result.Report.FilesTotal = entry.FilesTotal
	result.Report.FilesDelivered = entry.FilesDelivered
	result.Report.FilesWithheld = entry.FilesWithheld
	result.Patch = entry.Patch

	token, err := w.recordPullSyncPoint(ctx, spec, storeRepo.ID, tip, current.Version())
	if err != nil {
		return nil, err
	}
	result.NextSync = token

	w.audit.Record(ctx, spec.Repository, &store.AuditRecord{
		TenantID:      w.cfg.Tenant,
		OccurredAt:    w.deps.Now(),
		ActorUserID:   spec.UserID,
		ActorLabel:    string(spec.PolicyUserID),
		Action:        "pull.delivered",
		RepositoryID:  storeRepo.ID,
		Branch:        spec.Branch,
		PolicyVersion: current.Version(),
		RequestID:     spec.RequestID,
		TaskID:        task.ID,
		Detail: detail(map[string]string{
			"upstream_commit": tip,
			"from":            spec.FromCommit,
			// Recorded because "why did this pull take 40ms" and "why did this
			// one take 40 seconds" have the same answer, and an operator
			// reading the trail should not have to guess which happened.
			"reused_projection": strconv.FormatBool(cached),
		}),
	})

	return json.Marshal(result)
}

type pullCounts struct {
	total     int
	delivered int
	withheld  int
}

// filterForReader removes from a patch everything the subject may not read.
func (w *Worker) filterForReader(raw []byte, current *policy.Policy, spec taskspec.Pull, repo *store.Repository) ([]byte, pullCounts, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, pullCounts{}, nil
	}

	set, err := patch.Parse(raw)
	if errors.Is(err, patch.ErrEmpty) {
		return nil, pullCounts{}, nil
	}
	if err != nil {
		return nil, pullCounts{}, err
	}

	subject, err := current.Subject(spec.PolicyUserID)
	if err != nil {
		// The account was removed from the bundle while the task was queued.
		// Delivering nothing is the only safe reading of that.
		return nil, pullCounts{}, permanent("user_not_in_policy",
			"%s is no longer in the policy bundle", spec.PolicyUserID)
	}

	result, err := enforce.Pull(set, enforce.Options{
		Engine:  current,
		Repo:    repo.PolicyRepoID,
		Ref:     "refs/heads/" + spec.Branch,
		Subject: subject,
		Guards:  w.cfg.Guards,
	})
	if err != nil {
		return nil, pullCounts{}, err
	}

	return result.Patch, pullCounts{
		total:     len(result.Verdicts),
		delivered: len(result.Kept),
		withheld:  len(result.Dropped),
	}, nil
}

// storePullPatch compresses and stores a generated patch.
func (w *Worker) storePullPatch(ctx context.Context, filtered []byte) (protocol.Blob, error) {
	compressed, err := compress.Compress(filtered, protocol.EncodingZstd)
	if err != nil {
		return protocol.Blob{}, err
	}

	descriptor, err := w.deps.Blobs.Put(ctx, bytes.NewReader(compressed), "", w.cfg.MaxPatchBytes)
	if err != nil {
		return protocol.Blob{}, err
	}

	expires := w.deps.Now().Add(w.cfg.PullArtifactTTL)

	if _, err := w.deps.Store.Artifacts().Create(ctx, &store.Artifact{
		TenantID:         w.cfg.Tenant,
		Digest:           descriptor.Digest,
		Kind:             store.ArtifactPullPatch,
		Size:             descriptor.Size,
		UncompressedSize: int64(len(filtered)),
		Encoding:         protocol.EncodingZstd,
		Locator:          descriptor.Digest,
		CreatedAt:        w.deps.Now(),
		ExpiresAt:        &expires,
	}); err != nil {
		return protocol.Blob{}, err
	}

	return protocol.Blob{
		Digest:           descriptor.Digest,
		Size:             descriptor.Size,
		Encoding:         protocol.EncodingZstd,
		UncompressedSize: int64(len(filtered)),
	}, nil
}

// recordPullSyncPoint moves the workspace's recorded projection to the tip and
// mints the token the client will send back.
//
// Put, not CompareAndSet, unlike the push path. The stored point records the
// furthest projection nit has produced for this workspace; the client's token
// records what it actually holds, and a client that failed to apply keeps its
// older token and asks again from there. Requiring the two to match here would
// deadlock exactly that case.
func (w *Worker) recordPullSyncPoint(ctx context.Context, spec taskspec.Pull, repoID store.ID, commit, policyVersion string) (protocol.SyncToken, error) {
	if err := w.deps.Store.SyncPoints().Put(ctx, &store.SyncPoint{
		TenantID:       w.cfg.Tenant,
		WorkspaceID:    spec.WorkspaceID,
		RepositoryID:   repoID,
		Branch:         spec.Branch,
		UpstreamCommit: commit,
		PolicyVersion:  policyVersion,
		UpdatedAt:      w.deps.Now(),
	}); err != nil {
		return "", err
	}

	return w.deps.SyncTokens.Sign(syncPayload(spec.WorkspaceID, repoID, spec.Branch, commit, policyVersion, w.deps.Now().Unix()))
}

func syncPayload(workspace, repo store.ID, branch, commit, policyVersion string, issuedAt int64) synctoken.Payload {
	return synctoken.Payload{
		Workspace:      workspace,
		Repository:     repo,
		Branch:         branch,
		UpstreamCommit: commit,
		PolicyVersion:  policyVersion,
		IssuedAt:       issuedAt,
	}
}
