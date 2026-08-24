package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"sync"
)

// Memory is a blob store held in a map.
//
// It exists for the same reason policy.Static does: anything written against
// one of these seams needs a working one to test against, and the store that
// ships is a directory under internal/ that nothing outside this module can
// reach. Without this, every consumer writes the same forty lines, and each
// copy is a chance to get the digest wrong in a way their tests then bless.
//
// Not for production. Everything is resident, nothing is shared between
// processes, and a restart loses it — which is the opposite of what a blob
// store between a control plane and its workers has to be.
//
// It passes pkg/blob/blobtest, so a caller can trust it to fail the way the
// real one does: a mismatched digest is refused, a missing blob is ErrNotFound,
// an oversize payload is ErrTooLarge and stores nothing.
type Memory struct {
	mu    sync.RWMutex
	blobs map[string][]byte
}

// NewMemory returns an empty store.
func NewMemory() *Memory { return &Memory{blobs: map[string][]byte{}} }

func (m *Memory) Put(_ context.Context, r io.Reader, expected string, maxSize int64) (Descriptor, error) {
	if expected != "" {
		if err := ValidateDigest(expected); err != nil {
			return Descriptor{}, err
		}
	}

	source := r
	if maxSize > 0 {
		// One byte past the ceiling, so exceeding it is detectable rather than
		// indistinguishable from a payload ending exactly at the limit.
		source = io.LimitReader(r, maxSize+1)
	}

	content, err := io.ReadAll(source)
	if err != nil {
		return Descriptor{}, err
	}

	if maxSize > 0 && int64(len(content)) > maxSize {
		return Descriptor{}, ErrTooLarge
	}

	sum := sha256.Sum256(content)
	digest := DigestPrefix + hex.EncodeToString(sum[:])

	// Verified against what was read, never against what the caller announced.
	if expected != "" && digest != expected {
		return Descriptor{}, fmt.Errorf("%w: announced %s, received %s",
			ErrDigestMismatch, expected, digest)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	// Stored only once the content is known good, which is what makes this
	// atomic in the sense the contract means: no reader can observe a partial
	// write under a complete-looking digest.
	m.blobs[digest] = content

	return Descriptor{Digest: digest, Size: int64(len(content))}, nil
}

func (m *Memory) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	if err := ValidateDigest(digest); err != nil {
		return nil, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	content, ok := m.blobs[digest]
	if !ok {
		return nil, ErrNotFound
	}

	return io.NopCloser(bytes.NewReader(content)), nil
}

func (m *Memory) Stat(_ context.Context, digest string) (Descriptor, error) {
	if err := ValidateDigest(digest); err != nil {
		return Descriptor{}, err
	}

	m.mu.RLock()
	defer m.mu.RUnlock()

	content, ok := m.blobs[digest]
	if !ok {
		return Descriptor{}, ErrNotFound
	}

	return Descriptor{Digest: digest, Size: int64(len(content))}, nil
}

// Delete removes a blob, and does not mind if it was already gone: artifact
// collection deletes what it may already have deleted.
func (m *Memory) Delete(_ context.Context, digest string) error {
	if err := ValidateDigest(digest); err != nil {
		return err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	delete(m.blobs, digest)

	return nil
}

var _ Store = (*Memory)(nil)
