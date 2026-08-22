// Package blob stores patch payloads, addressed by the hash of their bytes.
//
// Content addressing is not decoration. It deduplicates repeated uploads of the
// same change, it lets a transfer resume without server-side session state, and
// it turns integrity checking into something the client can verify itself. It
// also makes the store trivially idempotent: writing the same bytes twice is
// the same write.
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

// limitedReader aborts with ErrTooLarge rather than truncating silently, which
// would store a corrupt patch under a digest that looks valid.
type limitedReader struct {
	r         io.Reader
	remaining int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.remaining < 0 {
		return 0, ErrTooLarge
	}

	if int64(len(p)) > l.remaining+1 {
		p = p[:l.remaining+1]
	}

	n, err := l.r.Read(p)
	l.remaining -= int64(n)

	if l.remaining < 0 {
		return n, ErrTooLarge
	}

	return n, err
}
