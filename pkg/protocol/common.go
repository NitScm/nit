package protocol

import "time"

// Version is the wire protocol version. It is sent on every request so a server
// can refuse a client it cannot serve, with a message telling the user to
// upgrade rather than a decoding error.
const Version = "1"

// Encoding names the compression applied to a patch payload.
type Encoding string

const (
	// EncodingZstd is the default. Patches are text and compress extremely
	// well; zstd beats gzip on both ratio and speed for this shape of data.
	EncodingZstd Encoding = "zstd"

	// EncodingNone is used for payloads too small to be worth compressing, and
	// for debugging.
	EncodingNone Encoding = "none"
)

// WorkspaceID identifies one checkout on one machine.
//
// It exists from day one even though nit ships assuming a single machine per
// developer: sync points are per workspace, and retrofitting that key later
// would mean migrating every stored sync point without knowing which machine
// each one belonged to. A developer with a laptop and a desktop simply gets two
// workspaces, each with its own sync point.
type WorkspaceID string

// SyncToken is an opaque handle on a sync point. Clients store it and send it
// back unchanged; only the server interprets it.
type SyncToken string

// Blob describes a payload transferred out of band, addressed by the hash of
// its compressed bytes.
//
// Content addressing is not decoration: it deduplicates repeated uploads, makes
// a transfer resumable, and turns integrity checking into a comparison the
// client can perform itself.
type Blob struct {
	// Digest is "sha256:<hex>" over the compressed bytes.
	Digest string `json:"digest"`

	// Size is the compressed length in bytes.
	Size int64 `json:"size"`

	Encoding Encoding `json:"encoding"`

	// UncompressedSize lets the receiver refuse a decompression bomb before
	// allocating anything.
	UncompressedSize int64 `json:"uncompressed_size"`
}

// Denial explains why one path of a patch was refused. It mirrors what the
// enforce package produced, flattened for the wire.
type Denial struct {
	Path   string `json:"path"`
	Action string `json:"action"`

	// Guard is set when the refusal came from a structural protection
	// (symlink, submodule, protected path) rather than an ordinary path rule.
	Guard string `json:"guard,omitempty"`

	// RuleID and Pattern identify what refused. Empty when nothing matched and
	// the default deny applied.
	RuleID  string `json:"rule_id,omitempty"`
	Pattern string `json:"pattern,omitempty"`

	Reason string `json:"reason"`

	// Description is the rule author's message to the developer. A denial
	// nobody can act on becomes a support ticket.
	Description string `json:"description,omitempty"`
}

// Error is the body returned with every non-2xx response.
type Error struct {
	// Code is a stable, machine-readable identifier. Clients branch on this,
	// never on Message.
	Code string `json:"code"`

	Message string `json:"message"`

	// Denials is populated when Code is CodeUnauthorizedPaths.
	Denials []Denial `json:"denials,omitempty"`

	// RetryAfter is set when the request may succeed later unchanged, for
	// example while another push holds the branch.
	RetryAfter time.Duration `json:"retry_after,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

// Error codes.
const (
	// CodeUnauthorizedPaths: the patch touches paths the author may not change.
	// The whole push was refused; Denials lists every problem, not just the
	// first, so the developer fixes them in one go.
	CodeUnauthorizedPaths = "unauthorized_paths"

	// CodeStaleSyncPoint: the sync token does not match the server's record.
	// The client must pull before pushing again.
	CodeStaleSyncPoint = "stale_sync_point"

	// CodeUnknownSyncPoint: the server has no sync point for this workspace,
	// repository and branch. The client must run a full clone.
	CodeUnknownSyncPoint = "unknown_sync_point"

	// CodeBranchBusy: another operation holds the branch. The request was not
	// queued and may be retried after RetryAfter.
	CodeBranchBusy = "branch_busy"

	// CodePatchTooLarge: the payload exceeds the configured limit.
	CodePatchTooLarge = "patch_too_large"

	// CodeUnsupportedVersion: the client protocol version is not served.
	CodeUnsupportedVersion = "unsupported_version"

	// CodeUnknownRepository: the repository is not under nit control.
	CodeUnknownRepository = "unknown_repository"

	// CodeConflict: the patch did not apply cleanly onto upstream. The task
	// report carries the conflicting paths.
	CodeConflict = "conflict"
)
