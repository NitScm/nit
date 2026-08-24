// Package pullcache is the contract for sharing a filtered projection between
// users whose read rights are identical.
//
// A pull clones, diffs sync_point..tip, and filters the result per user. Five
// hundred developers pulling after a release is five hundred diffs and five
// hundred filter passes for one upstream change — the cost that arrives on a
// release day rather than gradually.
//
// The observation that removes it: the output depends on the subject only
// through what the subject may read. Everyone in the same groups, with the same
// exemptions, at the same ref, under the same bundle, gets the same bytes. The
// first pull after a release can therefore serve all the others.
//
// # What is cached, and what is not
//
// The entry holds a *descriptor* — the digest, size and file counts — not the
// patch. The bytes already live in the blob store under that digest, so an
// entry is a few dozen bytes and the cache cannot grow into a memory problem
// however many profiles a policy has.
//
// # Why this is an interface
//
// The implementation that ships is per process, which collapses a release-day
// herd because the herd arrives within minutes on the same handful of workers.
// A deployment large enough to want one cache across a fleet — or one that
// already runs a shared store and would rather use it — implements this instead.
//
// **What must not be reimplemented is the key.** `policy.Profile` decides which
// users may share a projection, and its correctness is authorization
// correctness: two subjects with different rights sharing a profile is one
// person receiving another's files. An implementation stores what it is given
// and returns it under the same key; it never decides who may share with whom.
//
// # Why an entry cannot outlive what it points at
//
// A generated pull patch is an artifact with a TTL, and something eventually
// collects it. An entry naming a digest that has been swept would hand a client
// a patch it cannot fetch. Two things prevent that: entries expire well inside
// the artifact TTL, and Get verifies the blob is still there before returning a
// hit. A missing blob is a miss, not an error — the caller recomputes, which is
// exactly what it would have done anyway.
package pullcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"

	"github.com/NitScm/nit/pkg/protocol"
)

// Entry is what a hit returns: everything a pull result needs except the bytes.
type Entry struct {
	// Patch is nil when the projection was empty — a real outcome worth
	// caching, since a user who may read nothing that changed asks repeatedly
	// and gets nothing repeatedly.
	Patch *protocol.Blob

	FilesTotal     int
	FilesDelivered int
	FilesWithheld  int
}

// Key identifies one filtered projection.
//
// Repository, From and To fix the diff; Profile fixes what may be read of it.
// The profile already carries the policy version and the ref (see
// policy.Profile), so those are not repeated here — repeating them would invite
// the belief that they are what makes the key safe, when the profile is.
type Key struct {
	Repository string
	From       string
	To         string
	Profile    string
}

// Hash is the canonical identity of a key.
//
// Exported because an implementation storing entries in a table or an object
// store needs one column, and it must be *this* one. Every field takes part,
// length-prefixed so no two different keys can concatenate to the same bytes —
// an implementation that rolled its own and got that wrong would serve one
// rights profile's projection to another.
func (k Key) Hash() string {
	h := sha256.New()

	for _, part := range []string{k.Repository, k.From, k.To, k.Profile} {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte(":"))
		h.Write([]byte(part))
	}

	return hex.EncodeToString(h.Sum(nil))
}

// Store maps a projection key to the artifact that holds it.
//
// Two obligations beyond the signatures, and both are about what an
// implementation must *not* do.
//
// **A key is opaque and total.** Every field takes part in identity. An
// implementation that ignored one — or that truncated a key to fit a column —
// would serve one rights profile's projection to another, which is the failure
// this whole mechanism exists to prevent. Hash the whole thing.
//
// **A failure is a miss, never an error the caller must handle.** A pull that
// failed because a cache was unreachable would be worse than one that recomputed
// silently. Get reports its error so it can be logged; the caller treats it as a
// miss regardless.
type Store interface {
	// Get returns a cached projection. Absent is (Entry{}, false, nil).
	Get(ctx context.Context, key Key) (Entry, bool, error)

	// Put records a projection.
	//
	// The error is for logging. It must never fail the pull that produced the
	// entry: that work is already done and the client is already being served.
	Put(ctx context.Context, key Key, entry Entry) error
}
