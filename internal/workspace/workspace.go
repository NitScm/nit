// Package workspace manages a developer's local checkout: its state file, its
// credentials, and the git operations the CLI performs on it.
//
// The state a workspace carries is small but every field earns its place:
//
//	server      which nit deployment this checkout belongs to
//	repository  and branch, so no command needs arguments after clone
//	workspace   the server-side id, which keys the sync point
//	sync_token  the opaque token; the client's assertion of where it is
//	local_base  the local commit that corresponds to that token
//
// local_base is the piece a naive implementation forgets. A push has to be
// diffed from *somewhere*, and the developer's own commits are not upstream
// commits: the local tree is a filtered projection, so its hashes exist nowhere
// on the forge. The diff base is therefore the last local synchronization
// commit, and the token says which upstream commit that corresponds to.
package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/NitScm/nit/pkg/gitx"
	"github.com/NitScm/nit/pkg/protocol"
)

// Directory and file names inside a checkout.
const (
	Dir       = ".nit"
	StateFile = "state.json"
)

var (
	// ErrNotAWorkspace: the directory is not under nit control.
	ErrNotAWorkspace = errors.New("not a nit workspace; run: nit clone <repository>")

	// ErrDirty: the working tree has changes that an operation would clobber.
	ErrDirty = errors.New("the working tree has uncommitted changes")
)

// State is what a checkout remembers between commands.
type State struct {
	Server     string `json:"server"`
	Repository string `json:"repository"`
	Branch     string `json:"branch"`

	Workspace string `json:"workspace"`

	// SyncToken is opaque: stored, sent back, never parsed. Only the server
	// interprets it.
	SyncToken protocol.SyncToken `json:"sync_token"`

	// LocalBase is the local commit corresponding to SyncToken. Pushes are
	// diffed from here.
	LocalBase string `json:"local_base"`
}

// Workspace is an open checkout.
type Workspace struct {
	Root  string
	State State

	git  gitx.Git
	repo gitx.Repo
}

// Open loads the workspace containing dir, searching upwards as git does, so
// that commands work from any subdirectory.
func Open(ctx context.Context, git gitx.Git, dir string) (*Workspace, error) {
	root, err := findRoot(dir)
	if err != nil {
		return nil, err
	}

	state, err := loadState(root)
	if err != nil {
		return nil, err
	}

	repo, err := git.Open(ctx, root)
	if err != nil {
		return nil, fmt.Errorf("%s is not a git repository: %w", root, err)
	}

	return &Workspace{Root: root, State: state, git: git, repo: repo}, nil
}

// Init creates a workspace in an empty or new directory.
func Init(ctx context.Context, git gitx.Git, root string, state State) (*Workspace, error) {
	if err := os.MkdirAll(root, 0o750); err != nil {
		return nil, err
	}

	if err := runGitInit(ctx, root, state.Branch); err != nil {
		return nil, err
	}

	repo, err := git.Open(ctx, root)
	if err != nil {
		return nil, err
	}

	w := &Workspace{Root: root, State: state, git: git, repo: repo}

	// Save creates the .nit directory; the ignore file has to follow it.
	if err := w.Save(); err != nil {
		return nil, err
	}

	// Ignore nit's own directory so it never turns up in a patch, where the
	// server would refuse it as a protected path.
	if err := os.WriteFile(filepath.Join(root, Dir, ".gitignore"), []byte("*\n"), 0o600); err != nil {
		return nil, err
	}

	return w, nil
}

// Repo exposes the underlying git repository.
func (w *Workspace) Repo() gitx.Repo { return w.repo }

// Save writes the state file.
//
// Written to a temporary file and renamed: a state file truncated by a crash
// would leave the checkout unable to say where it is, which is unrecoverable
// without support.
func (w *Workspace) Save() error {
	dir := filepath.Join(w.Root, Dir)

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(w.State, "", "  ")
	if err != nil {
		return err
	}

	tmp := filepath.Join(dir, StateFile+".tmp")

	if err := os.WriteFile(tmp, append(encoded, '\n'), 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, filepath.Join(dir, StateFile))
}

// SyncMessage builds the commit message of a synchronization commit.
//
// The trailers make the workspace state recoverable from the git history alone,
// which survives a deleted or corrupted state file:
//
//	git log --grep='^Nit-Upstream-Commit:'
//
// The server remains the authority; the trailers are evidence, not truth.
func (w *Workspace) SyncMessage(upstreamCommit, policyVersion string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "nit: sync %s@%s\n\n", w.State.Repository, w.State.Branch)
	fmt.Fprintf(&b, "Nit-Upstream-Commit: %s\n", upstreamCommit)

	if policyVersion != "" {
		fmt.Fprintf(&b, "Nit-Policy-Version: %s\n", policyVersion)
	}

	fmt.Fprintf(&b, "Nit-Workspace: %s\n", w.State.Workspace)

	return b.String()
}

// ApplySync applies a delivered patch and records a synchronization commit.
//
// The complication is local work. A checkout's history is a synchronization
// commit followed by whatever the developer has committed since, and a pull has
// to end with a *new* synchronization commit carrying that local work on top —
// not with the delivered patch merged into the middle of it.
//
// So the patch is applied to the sync commit, not to HEAD, and the local
// commits are replayed onto the result. That is `git pull --rebase`, and it
// gives the same useful behaviour: a local commit whose change already landed
// upstream — which is exactly what happens after a push that was rebased — is
// recognized as already applied and dropped, instead of conflicting with
// itself.
//
// The order is deliberate throughout: apply, commit, replay, and only then
// persist the new token. Saving the token first and failing to apply would
// leave the client claiming a state it does not have, and every later diff
// would be computed against the wrong base.
func (w *Workspace) ApplySync(ctx context.Context, patch []byte, upstreamCommit, policyVersion string, next protocol.SyncToken) error {
	head, err := w.repo.ResolveRef(ctx, "HEAD")
	if err != nil {
		// No commit yet: this is the first synchronization of a fresh checkout.
		head = ""
	}

	base := w.State.LocalBase

	// Nothing local to replay: apply in place.
	if base == "" || head == "" || head == base {
		sync, err := w.applyAndCommit(ctx, patch, upstreamCommit, policyVersion)
		if err != nil {
			return err
		}

		return w.record(sync, next)
	}

	branch, err := w.currentBranch(ctx)
	if err != nil {
		return err
	}

	// Build the new synchronization commit on top of the old one, away from the
	// developer's commits.
	if _, err := runGit(ctx, w.Root, "checkout", "--quiet", "--detach", base); err != nil {
		return err
	}

	sync, err := w.applyAndCommit(ctx, patch, upstreamCommit, policyVersion)
	if err != nil {
		// Put the developer back where they were before giving up.
		_, _ = runGit(ctx, w.Root, "checkout", "--quiet", "--force", branch)
		return err
	}

	if _, err := runGit(ctx, w.Root, "checkout", "--quiet", branch); err != nil {
		return err
	}

	// Replay base..branch onto the new synchronization commit.
	if _, err := runGit(ctx, w.Root, "rebase", "--onto", sync, base, branch); err != nil {
		_, _ = runGit(ctx, w.Root, "rebase", "--abort")

		return fmt.Errorf("your local commits conflict with the upstream changes; "+
			"resolve them and pull again: %w", err)
	}

	return w.record(sync, next)
}

// applyAndCommit applies a patch to the working tree and commits it, returning
// the commit.
func (w *Workspace) applyAndCommit(ctx context.Context, patch []byte, upstreamCommit, policyVersion string) (string, error) {
	if len(patch) > 0 {
		if err := w.repo.Apply(ctx, patch, gitx.ApplyOptions{ThreeWay: false}); err != nil {
			return "", fmt.Errorf("the delivered patch did not apply: %w", err)
		}
	}

	return w.repo.CommitAll(ctx, w.SyncMessage(upstreamCommit, policyVersion), gitx.Author{
		Name:  "nit",
		Email: "nit@localhost",
	})
}

// record persists the new sync point.
func (w *Workspace) record(localBase string, next protocol.SyncToken) error {
	w.State.SyncToken = next
	w.State.LocalBase = localBase

	return w.Save()
}

// currentBranch returns the checked-out branch name.
func (w *Workspace) currentBranch(ctx context.Context) (string, error) {
	out, err := runGit(ctx, w.Root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return "", err
	}

	branch := strings.TrimSpace(out)
	if branch == "HEAD" {
		return "", errors.New("the workspace is in a detached HEAD state; check out a branch first")
	}

	return branch, nil
}

// RecordPush records that a push landed and left the workspace faithful.
func (w *Workspace) RecordPush(ctx context.Context, next protocol.SyncToken) error {
	head, err := w.repo.ResolveRef(ctx, "HEAD")
	if err != nil {
		return err
	}

	w.State.SyncToken = next
	w.State.LocalBase = head

	return w.Save()
}

// Diff returns the patch from the sync point to HEAD, in the exact form the
// server expects.
func (w *Workspace) Diff(ctx context.Context) ([]byte, error) {
	if w.State.LocalBase == "" {
		return nil, errors.New("this workspace has never synchronized; run: nit pull")
	}

	head, err := w.repo.ResolveRef(ctx, "HEAD")
	if err != nil {
		return nil, err
	}

	if head == w.State.LocalBase {
		return nil, nil
	}

	return w.repo.Diff(ctx, w.State.LocalBase, head)
}

// EnsureClean refuses to proceed when the working tree has changes an
// operation would clobber.
//
// git behaves the same way for the same reason: a pull that silently merged
// into uncommitted work would make the result impossible to untangle.
func (w *Workspace) EnsureClean(ctx context.Context) error {
	dirty, err := w.dirtyPaths(ctx)
	if err != nil {
		return err
	}
	if len(dirty) == 0 {
		return nil
	}

	shown := dirty
	if len(shown) > 5 {
		shown = shown[:5]
	}

	return fmt.Errorf("%w:\n  %s", ErrDirty, strings.Join(shown, "\n  "))
}

// Dirty reports whether the working tree has uncommitted changes.
func (w *Workspace) Dirty(ctx context.Context) (bool, error) {
	dirty, err := w.dirtyPaths(ctx)
	return len(dirty) > 0, err
}

func (w *Workspace) dirtyPaths(ctx context.Context) ([]string, error) {
	out, err := runGit(ctx, w.Root, "status", "--porcelain", "--untracked-files=normal")
	if err != nil {
		return nil, err
	}

	var paths []string

	for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			paths = append(paths, line)
		}
	}

	return paths, nil
}

// findRoot walks up from dir looking for a .nit directory.
func findRoot(dir string) (string, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}

	for {
		if info, err := os.Stat(filepath.Join(absolute, Dir, StateFile)); err == nil && !info.IsDir() {
			return absolute, nil
		}

		parent := filepath.Dir(absolute)
		if parent == absolute {
			return "", ErrNotAWorkspace
		}

		absolute = parent
	}
}

func loadState(root string) (State, error) {
	content, err := os.ReadFile(filepath.Join(root, Dir, StateFile))
	if err != nil {
		return State{}, ErrNotAWorkspace
	}

	var state State
	if err := json.Unmarshal(content, &state); err != nil {
		return State{}, fmt.Errorf("%s is corrupt: %w", filepath.Join(Dir, StateFile), err)
	}

	return state, nil
}
