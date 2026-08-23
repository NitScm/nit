// Package taskspec defines what the control plane hands a worker.
//
// It is deliberately separate from pkg/protocol: the wire types are a contract
// with clients and change slowly, while a task spec is an internal detail
// between two processes that always ship together. Keeping them apart means a
// worker gaining a field does not become a protocol change.
//
// A spec is self-contained. A worker never re-derives authorization, never
// re-reads the policy for a push, and never has to trust anything it was not
// given: the control plane already decided, and the spec records what it
// decided.
package taskspec

import (
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/store"
)

// Push is the instruction for a push worker.
type Push struct {
	RequestID string `json:"request_id"`

	Repository policy.RepoID `json:"repository"`
	Remote     string        `json:"remote"`
	Forge      string        `json:"forge"`
	Branch     string        `json:"branch"`

	// BaseCommit is the upstream commit the patch was computed against. The
	// worker checks it out, applies the patch there, then rebases onto the
	// current tip.
	BaseCommit string `json:"base_commit"`

	// PatchDigest names the *enforced* patch, not what the client uploaded. In
	// strip mode the two differ, and a worker that applied the client's version
	// would undo the whole authorization pass.
	PatchDigest   string            `json:"patch_digest"`
	PatchEncoding protocol.Encoding `json:"patch_encoding"`

	// Message is the author's commit message, exactly as they wrote it. The
	// worker sanitizes it before use: a message is free text, so it can contain
	// anything, including counterfeit Nit-* trailers.
	Message string `json:"message"`

	// DroppedFiles counts the sections the control plane removed in strip mode.
	//
	// It carries two jobs. The worker uses it to decide whether the workspace
	// stays a faithful projection of upstream: what lands differs from what the
	// author committed, so the sync point must not advance and the developer
	// has to pull to reconcile. It also becomes the Nit-Dropped trailer, which
	// is the only record on the forge that the commit is not what its author
	// wrote.
	DroppedFiles int `json:"dropped_files"`

	// AuthorName and AuthorEmail come from the authenticated identity, never
	// from anything the client claimed: the author field of a commit is free
	// text, and trusting it would let anyone attribute a change to a colleague.
	AuthorName  string `json:"author_name"`
	AuthorEmail string `json:"author_email"`

	UserID      store.ID `json:"user_id"`
	WorkspaceID store.ID `json:"workspace_id"`

	// PolicyUserID is the bundle identity of the author. The worker records it
	// on the audit line it writes: an audit trail that cannot name who acted is
	// not an audit trail.
	PolicyUserID policy.UserID `json:"policy_user_id"`

	PolicyVersion string `json:"policy_version"`
}

// Pull is the instruction for a pull worker.
//
// Unlike a push, a pull spec carries no patch: the worker computes the diff
// itself and applies the read rules to it, because what is readable depends on
// the bundle in force when the diff is produced, not when the request was made.
type Pull struct {
	RequestID string `json:"request_id"`

	Repository policy.RepoID `json:"repository"`
	Remote     string        `json:"remote"`
	Forge      string        `json:"forge"`
	Branch     string        `json:"branch"`

	// FromCommit is the client's sync point. Empty means the workspace has none
	// and needs a full filtered snapshot rather than an incremental diff.
	FromCommit string `json:"from_commit"`

	UserID       store.ID      `json:"user_id"`
	WorkspaceID  store.ID      `json:"workspace_id"`
	PolicyUserID policy.UserID `json:"policy_user_id"`

	// PolicyVersion is the bundle in force when the request was accepted,
	// recorded for the audit trail. The worker re-reads the current bundle to
	// filter, and reports which one it used.
	PolicyVersion string `json:"policy_version"`
}
