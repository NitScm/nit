package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// FS stores blobs as files under a root directory.
//
// This is the v1 backend. Object storage is the obvious next one, and the Store
// interface is what keeps that swap from touching anything else.
type FS struct {
	root string
}

// NewFS returns a filesystem-backed store rooted at dir.
func NewFS(dir string) (*FS, error) {
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("blob: create root: %w", err)
	}
	return &FS{root: dir}, nil
}

// path returns the on-disk location of a digest.
//
// The two-level fan-out keeps directory sizes manageable: a flat directory with
// hundreds of thousands of entries degrades badly on most filesystems.
func (s *FS) path(digest string) (string, error) {
	if err := ValidateDigest(digest); err != nil {
		return "", err
	}

	hex := digest[len(DigestPrefix):]

	return filepath.Join(s.root, hex[0:2], hex[2:4], hex), nil
}

// Put streams r to disk, hashing as it goes.
//
// The bytes land in a temporary file and are renamed into place only once the
// digest is known and verified. A reader that fails halfway therefore leaves no
// half-written blob that a later lookup could mistake for a complete one.
func (s *FS) Put(_ context.Context, r io.Reader, expected string, maxSize int64) (Descriptor, error) {
	if expected != "" {
		if err := ValidateDigest(expected); err != nil {
			return Descriptor{}, err
		}
	}

	tmpDir := filepath.Join(s.root, "tmp")
	if err := os.MkdirAll(tmpDir, 0o750); err != nil {
		return Descriptor{}, fmt.Errorf("blob: create temp dir: %w", err)
	}

	tmp, err := os.CreateTemp(tmpDir, "upload-*")
	if err != nil {
		return Descriptor{}, fmt.Errorf("blob: create temp file: %w", err)
	}

	tmpName := tmp.Name()

	// Any early return must not leave the temporary file behind.
	committed := false
	defer func() {
		tmp.Close()
		if !committed {
			os.Remove(tmpName)
		}
	}()

	var src io.Reader = r
	if maxSize > 0 {
		src = &limitedReader{r: r, remaining: maxSize}
	}

	hasher := sha256.New()

	size, err := io.Copy(io.MultiWriter(tmp, hasher), src)
	if err != nil {
		return Descriptor{}, err
	}
	if err := tmp.Sync(); err != nil {
		return Descriptor{}, fmt.Errorf("blob: sync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Descriptor{}, fmt.Errorf("blob: close: %w", err)
	}

	digest := DigestPrefix + hex.EncodeToString(hasher.Sum(nil))

	if expected != "" && digest != expected {
		return Descriptor{}, fmt.Errorf("%w: announced %s, received %s", ErrDigestMismatch, expected, digest)
	}

	final, err := s.path(digest)
	if err != nil {
		return Descriptor{}, err
	}

	if err := os.MkdirAll(filepath.Dir(final), 0o750); err != nil {
		return Descriptor{}, fmt.Errorf("blob: create shard: %w", err)
	}

	// Identical bytes are the same blob: an existing file needs no rewrite.
	if _, err := os.Stat(final); err == nil {
		return Descriptor{Digest: digest, Size: size}, nil
	}

	if err := os.Rename(tmpName, final); err != nil {
		return Descriptor{}, fmt.Errorf("blob: commit: %w", err)
	}

	committed = true

	return Descriptor{Digest: digest, Size: size}, nil
}

// Get opens a blob for reading.
func (s *FS) Get(_ context.Context, digest string) (io.ReadCloser, error) {
	path, err := s.path(digest)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("blob: open: %w", err)
	}

	return f, nil
}

// Stat returns a descriptor without reading the payload.
func (s *FS) Stat(_ context.Context, digest string) (Descriptor, error) {
	path, err := s.path(digest)
	if err != nil {
		return Descriptor{}, err
	}

	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return Descriptor{}, ErrNotFound
	}
	if err != nil {
		return Descriptor{}, fmt.Errorf("blob: stat: %w", err)
	}

	return Descriptor{Digest: digest, Size: info.Size()}, nil
}

// Delete removes a blob. Deleting something that is already gone is not an
// error: the garbage collector runs concurrently with itself often enough that
// treating it as one would only produce noise.
func (s *FS) Delete(_ context.Context, digest string) error {
	path, err := s.path(digest)
	if err != nil {
		return err
	}

	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("blob: delete: %w", err)
	}

	return nil
}

var _ Store = (*FS)(nil)
