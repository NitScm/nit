package filesystem

import (
	"bytes"
	"context"
	"errors"
	"github.com/NitScm/nit/pkg/blob"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()

	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	payload := []byte("diff --git a/a b/a\n")

	desc, err := s.Put(ctx, bytes.NewReader(payload), "", 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if desc.Digest != blob.Digest(payload) {
		t.Errorf("blob.Digest = %q, want %q", desc.Digest, blob.Digest(payload))
	}
	if desc.Size != int64(len(payload)) {
		t.Errorf("Size = %d, want %d", desc.Size, len(payload))
	}

	r, err := s.Get(ctx, desc.Digest)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Errorf("got %q, want %q", got, payload)
	}
}

func TestPutIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	payload := []byte("same bytes")

	first, err := s.Put(ctx, bytes.NewReader(payload), "", 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	second, err := s.Put(ctx, bytes.NewReader(payload), "", 0)
	if err != nil {
		t.Fatalf("Put again: %v", err)
	}

	if first.Digest != second.Digest {
		t.Errorf("digests differ: %q and %q", first.Digest, second.Digest)
	}
}

// An announced digest that does not match the bytes must be refused: storing it
// would poison every later lookup of that digest.
func TestPutRejectsDigestMismatch(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	wrong := blob.Digest([]byte("something else"))

	_, err := s.Put(ctx, strings.NewReader("actual content"), wrong, 0)
	if !errors.Is(err, blob.ErrDigestMismatch) {
		t.Fatalf("got %v, want blob.ErrDigestMismatch", err)
	}

	if _, err := s.Stat(ctx, wrong); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("a rejected upload left a blob behind: %v", err)
	}
}

func TestPutEnforcesMaxSize(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	_, err := s.Put(ctx, strings.NewReader("0123456789"), "", 4)
	if !errors.Is(err, blob.ErrTooLarge) {
		t.Fatalf("got %v, want blob.ErrTooLarge", err)
	}
}

func TestPutAtSizeLimitSucceeds(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.Put(ctx, strings.NewReader("1234"), "", 4); err != nil {
		t.Errorf("a payload exactly at the limit must be accepted: %v", err)
	}
}

// A failed upload must leave nothing behind, neither a blob nor a temporary
// file that a later run would have to guess about.
func TestFailedPutLeavesNoTemporaryFile(t *testing.T) {
	ctx := context.Background()

	root := t.TempDir()
	s, err := New(root)
	if err != nil {
		t.Fatalf("NewFS: %v", err)
	}

	_, err = s.Put(ctx, iotest{}, "", 0)
	if err == nil {
		t.Fatal("expected the reader error to surface")
	}

	entries, err := os.ReadDir(filepath.Join(root, "tmp"))
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read temp dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("temporary files left behind: %v", entries)
	}
}

type iotest struct{}

func (iotest) Read([]byte) (int, error) { return 0, errors.New("reader exploded") }

func TestGetMissing(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.Get(ctx, blob.Digest([]byte("absent"))); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("got %v, want blob.ErrNotFound", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	digest := blob.Digest([]byte("x"))

	if _, err := s.Put(ctx, strings.NewReader("x"), digest, 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, digest); err != nil {
		t.Errorf("deleting an absent blob must not be an error: %v", err)
	}
}

// Digests build file paths, so a malformed one must never reach the
// filesystem.
func TestValidateDigest(t *testing.T) {
	valid := blob.Digest([]byte("ok"))
	if err := blob.ValidateDigest(valid); err != nil {
		t.Errorf("blob.ValidateDigest(%q) = %v", valid, err)
	}

	invalid := []string{
		"",
		"sha256:",
		"md5:0123456789abcdef0123456789abcdef",
		"sha256:../../../etc/passwd",
		"sha256:" + strings.Repeat("g", 64),
		"sha256:" + strings.Repeat("A", 64), // uppercase is not canonical
		"sha256:" + strings.Repeat("a", 63),
	}

	for _, d := range invalid {
		if err := blob.ValidateDigest(d); err == nil {
			t.Errorf("blob.ValidateDigest(%q) accepted a malformed digest", d)
		}
	}
}

func TestPathTraversalIsRejected(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	if _, err := s.Get(ctx, "sha256:../../etc/passwd"); err == nil {
		t.Error("a traversal attempt must be rejected")
	}
}
