// Package gitx wraps the git operations nit needs, behind an interface.
//
// The implementation shells out to the git binary rather than using a pure-Go
// library. That is a deliberate trade: nit's correctness depends on three-way
// patch application, rename detection, binary deltas and --force-with-lease
// behaving exactly as upstream git does, on repositories that can be very
// large. Reimplementations of those are where the subtle differences live.
package gitx

import (
	"context"
	"errors"
	"time"
)

// ErrConflict is returned when a patch or a rebase does not apply cleanly.
var ErrConflict = errors.New("gitx: conflict")

// Author is the identity stamped on a commit.
//
// nit sets it from the authenticated session, never from what the client sent:
// the author field of a commit is free text, and trusting it would let anyone
// attribute a change to a colleague.
type Author struct {
	Name  string
	Email string
	When  time.Time
}

// CloneOptions configures a clone.
type CloneOptions struct {
	// Branch limits the clone to a single branch.
	Branch string

	// Depth, when non-zero, makes a shallow clone. Workers use it for pushes,
	// where only the tip is needed; it is unsafe for operations that must
	// resolve an older sync point, which is why it is not the default.
	Depth int

	// Bare clones without a working tree.
	Bare bool
}

// ApplyOptions configures patch application.
type ApplyOptions struct {
	// ThreeWay falls back to a three-way merge when a hunk does not apply
	// cleanly. It needs the blobs referenced by the patch's index lines to be
	// present, which is why patches are produced with --full-index and applied
	// inside a clone of upstream, where every blob exists.
	ThreeWay bool

	// Check validates the patch without touching the working tree.
	Check bool
}

// PushOptions configures a push to the forge.
type PushOptions struct {
	// ExpectedRemoteCommit enables --force-with-lease: the push fails unless
	// the remote branch is still at this commit.
	//
	// This is the real atomicity guarantee. The queue serializes work and
	// avoids wasted effort, but only the forge can arbitrate a race with a
	// change that did not come through nit at all.
	ExpectedRemoteCommit string
}

// Repo is an operation on a local clone.
type Repo interface {
	// Dir returns the working directory of the clone.
	Dir() string

	// ResolveRef returns the commit a ref points to.
	ResolveRef(ctx context.Context, ref string) (string, error)

	// Fetch updates remote-tracking refs.
	Fetch(ctx context.Context, remote string, refspecs ...string) error

	// Checkout switches the working tree to a commit or branch.
	Checkout(ctx context.Context, revision string) error

	// Diff returns the patch between two revisions, in the form nit's patch
	// package expects: binary deltas included, full index lines, rename
	// detection on.
	Diff(ctx context.Context, from, to string) ([]byte, error)

	// ChangedPaths lists the paths touched between two revisions. It is the
	// cheap pre-check used before producing a full patch.
	ChangedPaths(ctx context.Context, from, to string) ([]string, error)

	// Apply applies a patch to the working tree.
	Apply(ctx context.Context, patch []byte, opts ApplyOptions) error

	// Rebase replays the current branch onto another commit.
	//
	// The committer is explicit because a rebase rewrites commits. Author lines
	// survive it; committer lines do not, so without this the identity stamped
	// on a rebased push is whoever runs the worker.
	//
	// It returns ErrConflict when the replay does not apply cleanly, having
	// first aborted so the clone is left usable. A conflict is not an error to
	// retry: the same patch will conflict again, and only the author can
	// resolve it.
	Rebase(ctx context.Context, onto string, committer Author) error

	// EmptyTree returns the hash of the empty tree, creating the object if the
	// repository does not have it yet.
	//
	// Diffing against it is how a full snapshot is produced: the patch from the
	// empty tree to a commit creates every file, which is exactly what a
	// workspace with no sync point needs.
	EmptyTree(ctx context.Context) (string, error)

	// CommitAll stages every change in the working tree and creates a single
	// commit. nit flattens a submission into one commit, so this is all the
	// commit surface a worker needs.
	CommitAll(ctx context.Context, message string, author Author) (string, error)

	// Push publishes a branch to a remote.
	Push(ctx context.Context, remote, branch string, opts PushOptions) error

	// Close releases the clone.
	Close() error
}

// Git creates clones.
type Git interface {
	// Clone fetches remote into dir and returns a Repo for it.
	Clone(ctx context.Context, remote, dir string, opts CloneOptions) (Repo, error)

	// Open returns a Repo for an existing clone.
	Open(ctx context.Context, dir string) (Repo, error)

	// Version reports the git version in use, for diagnostics and for refusing
	// to run against a build too old to support the flags nit relies on.
	Version(ctx context.Context) (string, error)
}
