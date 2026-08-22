// Package flow implements the three operations a developer performs: clone,
// pull and push.
//
// They live here rather than in cmd/nit because they are the interesting part
// of the client and they are worth testing against a real server. A CLI whose
// logic sits in main() can only be exercised by running the binary, which in
// practice means it is not exercised at all.
//
// Each flow is the same shape: ask the server, wait for the worker, then move
// local state — in that order, never the other way round. Recording a new sync
// point before the change it describes has been applied is precisely how a
// workspace comes to claim a state it does not have.
package flow

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/NitScm/nit/internal/client"
	"github.com/NitScm/nit/internal/compress"
	"github.com/NitScm/nit/internal/workspace"
	"github.com/NitScm/nit/pkg/gitx"
	"github.com/NitScm/nit/pkg/protocol"
)

// Reporter receives progress, so a command can show something while a clone
// runs on the server. A nil Reporter is silent.
type Reporter interface {
	Progress(format string, args ...any)
}

// Runner performs the flows.
type Runner struct {
	Client   *client.Client
	Git      gitx.Git
	Reporter Reporter

	// NewRequestID generates the idempotency key of a submission. It is
	// injectable so tests are deterministic; production uses random ids.
	NewRequestID func() string
}

func (r *Runner) progress(format string, args ...any) {
	if r.Reporter != nil {
		r.Reporter.Progress(format, args...)
	}
}

func (r *Runner) requestID() string {
	if r.NewRequestID != nil {
		return r.NewRequestID()
	}
	return newRequestID()
}

// CloneOptions configures a clone.
type CloneOptions struct {
	Server     string
	Repository string
	Branch     string
	Directory  string
	Label      string
}

// CloneResult reports what a clone delivered.
type CloneResult struct {
	Directory string
	Report    protocol.PullReport
}

// Clone creates a workspace and fills it with a filtered snapshot.
//
// The snapshot is a pull with no sync token: the server has no record of this
// workspace, so it produces the diff from the empty tree, which creates every
// file the developer may read. No separate code path is needed for the first
// synchronization.
func (r *Runner) Clone(ctx context.Context, opts CloneOptions) (*CloneResult, error) {
	if opts.Repository == "" {
		return nil, errors.New("no repository named")
	}

	dir := opts.Directory
	if dir == "" {
		dir = opts.Repository
	}

	if err := ensureEmptyDirectory(dir); err != nil {
		return nil, err
	}

	branch := opts.Branch

	if branch == "" {
		repos, err := r.Client.Repositories(ctx)
		if err != nil {
			return nil, err
		}

		for _, repo := range repos {
			if repo.ID == opts.Repository {
				branch = repo.DefaultBranch
			}
		}

		if branch == "" {
			return nil, fmt.Errorf("repository %q is not available to you", opts.Repository)
		}
	}

	r.progress("registering the workspace")

	label := opts.Label
	if label == "" {
		label = hostname()
	}

	registered, err := r.Client.CreateWorkspace(ctx, label)
	if err != nil {
		return nil, err
	}

	ws, err := workspace.Init(ctx, r.Git, dir, workspace.State{
		Server:     opts.Server,
		Repository: opts.Repository,
		Branch:     branch,
		Workspace:  registered.ID,
	})
	if err != nil {
		return nil, err
	}

	report, err := r.pull(ctx, ws)
	if err != nil {
		// The checkout is unusable and half-created; leaving it behind would
		// make the obvious retry ("nit clone" again) fail on a non-empty
		// directory.
		os.RemoveAll(dir)
		return nil, err
	}

	absolute, _ := filepath.Abs(dir)

	return &CloneResult{Directory: absolute, Report: report}, nil
}

// Pull fetches and applies the filtered upstream changes.
func (r *Runner) Pull(ctx context.Context, ws *workspace.Workspace) (protocol.PullReport, error) {
	if err := ws.EnsureClean(ctx); err != nil {
		return protocol.PullReport{}, err
	}

	return r.pull(ctx, ws)
}

func (r *Runner) pull(ctx context.Context, ws *workspace.Workspace) (protocol.PullReport, error) {
	response, err := r.Client.Pull(ctx, protocol.PullRequest{
		RequestID:  r.requestID(),
		Repository: ws.State.Repository,
		Branch:     ws.State.Branch,
		Workspace:  protocol.WorkspaceID(ws.State.Workspace),
		Sync:       ws.State.SyncToken,
	})
	if err != nil {
		return protocol.PullReport{}, err
	}

	if response.UpToDate {
		return protocol.PullReport{}, nil
	}

	r.progress("waiting for the server to prepare the changes")

	task, err := r.Client.WaitForTask(ctx, response.TaskID, r.reportProgress)
	if err != nil {
		return protocol.PullReport{}, err
	}

	if task.State != protocol.TaskSucceeded {
		return protocol.PullReport{}, taskFailure(task)
	}
	if task.PullResult == nil {
		return protocol.PullReport{}, errors.New("the server returned no pull result")
	}

	result := task.PullResult

	var patch []byte

	if result.Patch != nil {
		r.progress("downloading %d bytes", result.Patch.Size)

		compressed, err := r.Client.FetchPatch(ctx, task.ID)
		if err != nil {
			return result.Report, err
		}

		patch, err = compress.Decompress(compressed, result.Patch.Encoding, 0)
		if err != nil {
			return result.Report, err
		}
	}

	if err := ws.ApplySync(ctx, patch, result.Report.UpstreamCommit, result.Report.PolicyVersion, result.NextSync); err != nil {
		return result.Report, err
	}

	return result.Report, nil
}

// PushOptions configures a push.
type PushOptions struct {
	Message string

	// Check runs authorization without submitting anything.
	Check bool

	// DropUnauthorized asks the server to strip what it refuses instead of
	// rejecting the whole push. It must be an explicit choice: what lands then
	// differs from what the author committed.
	DropUnauthorized bool
}

// PushResult reports what a push did.
type PushResult struct {
	Report protocol.PushReport

	// UpstreamCommit is empty for a check.
	UpstreamCommit string

	// NeedsPull is true when the push landed but the workspace is no longer a
	// faithful projection — the branch moved, or sections were stripped.
	NeedsPull bool
}

// Push submits the local changes.
func (r *Runner) Push(ctx context.Context, ws *workspace.Workspace, opts PushOptions) (*PushResult, error) {
	if opts.Message == "" && !opts.Check {
		return nil, errors.New("a message is required; use -m")
	}

	patch, err := ws.Diff(ctx)
	if err != nil {
		return nil, err
	}
	if len(patch) == 0 {
		return nil, errors.New("nothing to push")
	}

	if dirty, err := ws.Dirty(ctx); err != nil {
		return nil, err
	} else if dirty {
		// Not an error: git pushes commits, not working trees, and so does nit.
		// Saying so avoids the "where did my change go?" support ticket.
		r.progress("note: uncommitted changes are not included")
	}

	compressed, err := compress.Compress(patch, protocol.EncodingZstd)
	if err != nil {
		return nil, err
	}

	r.progress("uploading %d bytes", len(compressed))

	descriptor, err := r.Client.UploadPatch(ctx, compressed, "")
	if err != nil {
		return nil, err
	}

	descriptor.Encoding = protocol.EncodingZstd
	descriptor.UncompressedSize = int64(len(patch))

	mode := protocol.PushModeReject
	if opts.DropUnauthorized {
		mode = protocol.PushModeStrip
	}

	response, err := r.Client.Push(ctx, protocol.PushRequest{
		RequestID:  r.requestID(),
		Repository: ws.State.Repository,
		Branch:     ws.State.Branch,
		Workspace:  protocol.WorkspaceID(ws.State.Workspace),
		BaseSync:   ws.State.SyncToken,
		Message:    opts.Message,
		Patch:      descriptor,
		Mode:       mode,
		DryRun:     opts.Check,
	})
	if err != nil {
		return nil, err
	}

	if opts.Check || !response.Accepted {
		return &PushResult{Report: response.Report}, nil
	}

	if response.QueuePosition > 0 {
		r.progress("queued behind %d operation(s) on %s", response.QueuePosition, ws.State.Branch)
	}

	task, err := r.Client.WaitForTask(ctx, response.TaskID, r.reportProgress)
	if err != nil {
		return nil, err
	}

	if task.State != protocol.TaskSucceeded {
		return nil, taskFailure(task)
	}
	if task.PushResult == nil {
		return nil, errors.New("the server returned no push result")
	}

	result := &PushResult{
		Report:         response.Report,
		UpstreamCommit: task.PushResult.UpstreamCommit,
		NeedsPull:      task.PushResult.NextSync == "",
	}

	// No token means the workspace stopped being a faithful projection: the
	// branch moved under the push, or sections were stripped. The local state
	// must not advance, and the developer has to pull.
	if !result.NeedsPull {
		if err := ws.RecordPush(ctx, task.PushResult.NextSync); err != nil {
			return result, err
		}
	}

	return result, nil
}

func (r *Runner) reportProgress(state protocol.TaskState, position int) {
	switch {
	case state == protocol.TaskQueued && position > 0:
		r.progress("queued, %d ahead", position)
	case state == protocol.TaskQueued:
		r.progress("queued")
	case state == protocol.TaskRunning:
		r.progress("running")
	}
}

// taskFailure turns a failed task into the error the user sees.
func taskFailure(task *protocol.Task) error {
	if task.Error != nil {
		return task.Error
	}

	return fmt.Errorf("the task ended as %s", task.State)
}

func ensureEmptyDirectory(dir string) error {
	entries, err := os.ReadDir(dir)

	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil
	case err != nil:
		return err
	case len(entries) > 0:
		return fmt.Errorf("%s is not empty", dir)
	default:
		return nil
	}
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "workspace"
	}
	return name
}
