package blob

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newStore(t *testing.T) *FS {
	t.Helper()

	s, err := NewFS(t.TempDir())
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

	if desc.Digest != Digest(payload) {
		t.Errorf("Digest = %q, want %q", desc.Digest, Digest(payload))
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

	wrong := Digest([]byte("something else"))

	_, err := s.Put(ctx, strings.NewReader("actual content"), wrong, 0)
	if !errors.Is(err, ErrDigestMismatch) {
		t.Fatalf("got %v, want ErrDigestMismatch", err)
	}

	if _, err := s.Stat(ctx, wrong); !errors.Is(err, ErrNotFound) {
		t.Errorf("a rejected upload left a blob behind: %v", err)
	}
}

func TestPutEnforcesMaxSize(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	_, err := s.Put(ctx, strings.NewReader("0123456789"), "", 4)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("got %v, want ErrTooLarge", err)
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
	s, err := NewFS(root)
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

	if _, err := s.Get(ctx, Digest([]byte("absent"))); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestDeleteIsIdempotent(t *testing.T) {
	ctx := context.Background()
	s := newStore(t)

	digest := Digest([]byte("x"))

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
	valid := Digest([]byte("ok"))
	if err := ValidateDigest(valid); err != nil {
		t.Errorf("ValidateDigest(%q) = %v", valid, err)
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
		if err := ValidateDigest(d); err == nil {
			t.Errorf("ValidateDigest(%q) accepted a malformed digest", d)
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
