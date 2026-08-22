// Package protocol defines the wire contract between the nit CLI, the control
// plane and the workers.
//
// It is shared by client and server on purpose: a single Go module means the
// two can never disagree about a field name, and the CLI can pre-validate a
// request locally before spending a round trip.
//
// # Sync points
//
// The single most important idea in this package is the sync token.
//
// Once read filtering is in play, a developer's local repository is a filtered
// projection of the upstream one: files they may not read are simply absent, so
// their trees differ and their commit hashes differ. A local commit hash does
// not exist upstream, and asking the server to "diff from my last commit" is
// therefore meaningless.
//
// Instead, the server records a sync point per (workspace, repository, branch):
// the upstream commit whose filtered projection produced the developer's
// current state. The client holds an opaque token for it. Pull means "give me
// the filtered diff between my sync point and upstream HEAD"; push means "apply
// this patch on top of my sync point, then rebase it onto upstream HEAD".
//
// The token is opaque by design. It encodes the upstream commit, the workspace
// and the policy version that produced the projection, and clients must treat
// it as a cookie: store it, send it back, never parse it. That leaves the
// server free to change what a sync point contains — and it prevents a client
// from claiming a sync point it was never given.
package protocol
