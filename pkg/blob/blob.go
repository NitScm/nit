// Package blob is the contract for storing patch payloads, addressed by the
// hash of their bytes.
//
// Content addressing is not decoration. It deduplicates repeated uploads of the
// same change, it lets a transfer resume without server-side session state, and
// it turns integrity checking into something the client can verify itself. It
// also makes the store trivially idempotent: writing the same bytes twice is
// the same write.
//
// Like pkg/store, this package declares an interface and performs no IO of its
// own. The filesystem implementation lives in internal/blob, and the interface
// interface is an extension point: an operator whose infrastructure is not a
// filesystem —
// object storage, a content-addressed store they already run — implements
// Store and passes it in. See docs/EXTENSIONS.md.
package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// DigestPrefix is the algorithm marker carried by every digest.
const DigestPrefix = "sha256:"

var (
	// ErrNotFound: no blob with this digest.
	ErrNotFound = errors.New("blob: not found")

	// ErrDigestMismatch: the bytes received do not hash to the digest the
	// caller announced. The upload is discarded — an artifact whose content
	// does not match its name would poison every later lookup.
	ErrDigestMismatch = errors.New("blob: digest mismatch")

	// ErrTooLarge: the payload exceeded the configured ceiling.
	ErrTooLarge = errors.New("blob: payload too large")
)

// Descriptor identifies a stored blob.
type Descriptor struct {
	Digest string
	Size   int64
}

// Store persists opaque byte payloads.
//
// An implementation has three obligations beyond the signatures. Put must be
// atomic: a reader must never observe a partially written blob under a digest
// that looks complete, because the digest is what the rest of the system trusts
// instead of re-reading the bytes. Put must verify what it stored against
// expected when one is given, rather than trusting the caller. And Get must
// return ErrNotFound rather than empty content for a digest that was never
// written — a push whose patch is missing has to fail, not apply nothing.
type Store interface {
	// Put streams r into the store and returns its descriptor.
	//
	// expected, when non-empty, is the digest the caller announced; a mismatch
	// discards the upload and returns ErrDigestMismatch. maxSize, when
	// positive, aborts with ErrTooLarge instead of writing an unbounded
	// payload to disk.
	Put(ctx context.Context, r io.Reader, expected string, maxSize int64) (Descriptor, error)

	// Get opens a blob for reading. The caller closes it.
	Get(ctx context.Context, digest string) (io.ReadCloser, error)

	// Stat returns a blob's descriptor without reading it.
	Stat(ctx context.Context, digest string) (Descriptor, error)

	Delete(ctx context.Context, digest string) error
}

// Digest computes the digest of a byte slice, in the form the store uses.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return DigestPrefix + hex.EncodeToString(sum[:])
}

// ValidateDigest checks that a digest is well formed. Digests arrive from
// clients and are used to build file paths, so a malformed one must be rejected
// before it reaches the filesystem.
func ValidateDigest(digest string) error {
	if len(digest) != len(DigestPrefix)+64 {
		return fmt.Errorf("blob: malformed digest %q", digest)
	}
	if digest[:len(DigestPrefix)] != DigestPrefix {
		return fmt.Errorf("blob: unsupported digest algorithm in %q", digest)
	}

	for _, c := range digest[len(DigestPrefix):] {
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')
		if !isHex {
			return fmt.Errorf("blob: malformed digest %q", digest)
		}
	}

	return nil
}
