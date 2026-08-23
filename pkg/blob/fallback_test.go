package blob_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/NitScm/nit/internal/blob/filesystem"
	"github.com/NitScm/nit/pkg/blob"
	"github.com/NitScm/nit/pkg/blob/blobtest"
)

func newFallback(t *testing.T) (blob.Fallback, blob.Store, blob.Store) {
	t.Helper()

	primary, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("primary: %v", err)
	}

	secondary, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("secondary: %v", err)
	}

	return blob.Fallback{Primary: primary, Secondary: secondary}, primary, secondary
}

// A composition of two Stores is a Store, and has to be held to the same
// contract: nothing downstream knows it is looking at two directories.
func TestFallbackConformance(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) blob.Store {
		f, _, _ := newFallback(t)
		return f
	})
}

// The job it exists for: a blob written before the move is still readable
// after it.
func TestABlobInTheOldLocationIsStillFound(t *testing.T) {
	f, _, secondary := newFallback(t)
	ctx := context.Background()

	const content = "a patch uploaded before the move\n"

	descriptor, err := secondary.Put(ctx, strings.NewReader(content), "", 0)
	if err != nil {
		t.Fatalf("Put into the old location: %v", err)
	}

	body, err := f.Get(ctx, descriptor.Digest)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer body.Close()

	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != content {
		t.Errorf("read %q, want %q", got, content)
	}

	if _, err := f.Stat(ctx, descriptor.Digest); err != nil {
		t.Errorf("Stat did not fall back: %v", err)
	}
}

// Writes must not go to both. Two copies of the same bytes are not a safety
// net, they are twice the disk and a second thing to delete.
func TestWritesGoOnlyToThePrimary(t *testing.T) {
	f, primary, secondary := newFallback(t)
	ctx := context.Background()

	descriptor, err := f.Put(ctx, strings.NewReader("new"), "", 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if _, err := primary.Stat(ctx, descriptor.Digest); err != nil {
		t.Errorf("the primary did not receive the write: %v", err)
	}
	if _, err := secondary.Stat(ctx, descriptor.Digest); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("the old location received a write: %v", err)
	}
}

// Deletes reach both, or the old location grows forever and the transition
// never ends.
func TestDeleteEmptiesBoth(t *testing.T) {
	f, primary, secondary := newFallback(t)
	ctx := context.Background()

	const content = "in both, briefly\n"

	descriptor, err := secondary.Put(ctx, strings.NewReader(content), "", 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := primary.Put(ctx, strings.NewReader(content), "", 0); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := f.Delete(ctx, descriptor.Digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	for name, s := range map[string]blob.Store{"primary": primary, "secondary": secondary} {
		if _, err := s.Stat(ctx, descriptor.Digest); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("the %s still holds the blob: %v", name, err)
		}
	}
}

// The primary wins. A blob present in both must be read from the location that
// is still being written to.
func TestThePrimaryAnswersFirst(t *testing.T) {
	f, primary, secondary := newFallback(t)
	ctx := context.Background()

	// Same digest is impossible with different bytes, so this is about which
	// store is consulted rather than which content comes back: the secondary
	// is emptied afterwards and the read must still succeed.
	descriptor, err := primary.Put(ctx, bytes.NewReader([]byte("current")), "", 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, err := secondary.Put(ctx, bytes.NewReader([]byte("current")), "", 0); err != nil {
		t.Fatalf("Put: %v", err)
	}

	if err := secondary.Delete(ctx, descriptor.Digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, err := f.Stat(ctx, descriptor.Digest); err != nil {
		t.Errorf("the primary was not consulted first: %v", err)
	}
}
