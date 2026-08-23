// Package blobtest is a conformance suite every blob.Store implementation must
// pass.
//
// It exists for the same reason pkg/store/storetest does. A blob store holds
// the patch that is about to be applied to somebody's repository, and it is
// trusted by digest rather than re-read: nothing downstream checks the bytes
// again. So the obligations that matter are not in the signatures — that a Put
// is atomic, that a mismatched digest is refused rather than stored, that a
// missing blob is an error rather than empty content — and an implementation
// that gets one of them wrong fails in a way that looks like a working system
// applying the wrong change.
//
// Run it against your implementation and the contract holds. Do not paraphrase
// it into your own tests: the point is that every backend is held to the same
// assertions, in the same words.
package blobtest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/NitScm/nit/pkg/blob"
)

// Factory builds a fresh, empty store for one test.
type Factory func(t *testing.T) blob.Store

// Run executes the whole suite against an implementation.
func Run(t *testing.T, newStore Factory) {
	t.Helper()

	t.Run("PutGetRoundTrip", func(t *testing.T) { testRoundTrip(t, newStore) })
	t.Run("PutIsIdempotent", func(t *testing.T) { testIdempotent(t, newStore) })
	t.Run("PutRefusesADigestMismatch", func(t *testing.T) { testDigestMismatch(t, newStore) })
	t.Run("PutRefusesAnOversizePayload", func(t *testing.T) { testMaxSize(t, newStore) })
	t.Run("PutAcceptsExactlyTheLimit", func(t *testing.T) { testAtLimit(t, newStore) })
	t.Run("AFailedPutStoresNothing", func(t *testing.T) { testFailedPutStoresNothing(t, newStore) })
	t.Run("GetOfAMissingBlobIsNotFound", func(t *testing.T) { testGetMissing(t, newStore) })
	t.Run("StatOfAMissingBlobIsNotFound", func(t *testing.T) { testStatMissing(t, newStore) })
	t.Run("StatMatchesWhatWasPut", func(t *testing.T) { testStat(t, newStore) })
	t.Run("DeleteIsIdempotent", func(t *testing.T) { testDeleteIdempotent(t, newStore) })
	t.Run("DeleteRemovesOnlyItsTarget", func(t *testing.T) { testDeleteIsNarrow(t, newStore) })
	t.Run("EmptyContentIsStorable", func(t *testing.T) { testEmptyContent(t, newStore) })
	t.Run("MalformedDigestsAreRefused", func(t *testing.T) { testMalformedDigests(t, newStore) })
	t.Run("LargePayloadSurvivesIntact", func(t *testing.T) { testLargePayload(t, newStore) })
	t.Run("ConcurrentPutsOfTheSameBytes", func(t *testing.T) { testConcurrentPuts(t, newStore) })
}

func put(t *testing.T, s blob.Store, content string) blob.Descriptor {
	t.Helper()

	descriptor, err := s.Put(context.Background(), strings.NewReader(content), "", 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	return descriptor
}

func read(t *testing.T, s blob.Store, digest string) string {
	t.Helper()

	r, err := s.Get(context.Background(), digest)
	if err != nil {
		t.Fatalf("Get(%s): %v", digest, err)
	}
	defer r.Close()

	content, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	return string(content)
}

func testRoundTrip(t *testing.T, newStore Factory) {
	s := newStore(t)

	const content = "diff --git a/src/app.go b/src/app.go\n"

	descriptor := put(t, s, content)

	if descriptor.Digest != blob.Digest([]byte(content)) {
		t.Errorf("Digest = %s, want the hash of the bytes stored", descriptor.Digest)
	}
	if descriptor.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", descriptor.Size, len(content))
	}
	if got := read(t, s, descriptor.Digest); got != content {
		t.Errorf("read back %q, want %q", got, content)
	}
}

// Content addressing means writing the same bytes twice is the same write. An
// implementation that appended, or that failed the second time, would break
// every retried upload.
func testIdempotent(t *testing.T, newStore Factory) {
	s := newStore(t)

	const content = "the same bytes\n"

	first := put(t, s, content)
	second := put(t, s, content)

	if first.Digest != second.Digest {
		t.Errorf("digests differ across two identical Puts: %s and %s", first.Digest, second.Digest)
	}
	if second.Size != int64(len(content)) {
		t.Errorf("Size = %d after the second Put, want %d", second.Size, len(content))
	}
	if got := read(t, s, second.Digest); got != content {
		t.Errorf("read back %q after two Puts, want %q", got, content)
	}
}

// A blob whose content does not match its name would poison every later lookup:
// nothing downstream re-reads the bytes, so the digest is the only check there
// is.
func testDigestMismatch(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	announced := blob.Digest([]byte("what the caller promised"))

	_, err := s.Put(ctx, strings.NewReader("what the caller sent"), announced, 0)
	if !errors.Is(err, blob.ErrDigestMismatch) {
		t.Fatalf("Put with a wrong digest = %v, want ErrDigestMismatch", err)
	}

	// And the announced digest must not now resolve to anything.
	if _, err := s.Get(ctx, announced); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get after a mismatch = %v, want ErrNotFound; the bad upload was kept", err)
	}
}

func testMaxSize(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	content := strings.Repeat("x", 100)

	_, err := s.Put(ctx, strings.NewReader(content), "", 50)
	if !errors.Is(err, blob.ErrTooLarge) {
		t.Fatalf("Put over the limit = %v, want ErrTooLarge", err)
	}

	// Truncating instead of refusing would store a corrupt patch under a digest
	// that verifies, which is the worse of the two failures.
	if _, err := s.Get(ctx, blob.Digest([]byte(content[:50]))); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("a truncated payload was stored: %v", err)
	}
}

// The boundary itself, because "at most" and "less than" are one character
// apart and a patch exactly at the configured ceiling is a legitimate one.
func testAtLimit(t *testing.T, newStore Factory) {
	s := newStore(t)

	content := strings.Repeat("x", 64)

	descriptor, err := s.Put(context.Background(), strings.NewReader(content), "", 64)
	if err != nil {
		t.Fatalf("Put of exactly the limit: %v", err)
	}
	if descriptor.Size != 64 {
		t.Errorf("Size = %d, want 64", descriptor.Size)
	}
}

// Whatever a failed Put leaves behind, it must not be reachable. A partially
// written blob under a digest that looks complete is not a corrupt file: it is
// a patch that passes verification and applies the wrong thing.
func testFailedPutStoresNothing(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	content := strings.Repeat("y", 200)
	digest := blob.Digest([]byte(content))

	if _, err := s.Put(ctx, strings.NewReader(content), "", 100); !errors.Is(err, blob.ErrTooLarge) {
		t.Fatalf("Put = %v, want ErrTooLarge", err)
	}

	if _, err := s.Get(ctx, digest); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get after a failed Put = %v, want ErrNotFound", err)
	}
	if _, err := s.Stat(ctx, digest); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat after a failed Put = %v, want ErrNotFound", err)
	}
}

// A push whose patch has gone missing has to fail. Returning empty content
// would report success for a change that never landed.
func testGetMissing(t *testing.T, newStore Factory) {
	s := newStore(t)

	_, err := s.Get(context.Background(), blob.Digest([]byte("never stored")))
	if !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get of an absent blob = %v, want ErrNotFound", err)
	}
}

func testStatMissing(t *testing.T, newStore Factory) {
	s := newStore(t)

	_, err := s.Stat(context.Background(), blob.Digest([]byte("never stored")))
	if !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Stat of an absent blob = %v, want ErrNotFound", err)
	}
}

func testStat(t *testing.T, newStore Factory) {
	s := newStore(t)

	const content = "some bytes to measure\n"

	stored := put(t, s, content)

	got, err := s.Stat(context.Background(), stored.Digest)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}

	if got.Digest != stored.Digest {
		t.Errorf("Stat digest = %s, want %s", got.Digest, stored.Digest)
	}
	if got.Size != int64(len(content)) {
		t.Errorf("Stat size = %d, want %d", got.Size, len(content))
	}
}

// Artifact collection deletes blobs it may already have deleted, so a second
// Delete is a normal event rather than an error.
func testDeleteIdempotent(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	stored := put(t, s, "to be removed\n")

	if err := s.Delete(ctx, stored.Digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if err := s.Delete(ctx, stored.Digest); err != nil {
		t.Errorf("second Delete: %v, want nil", err)
	}
	if err := s.Delete(ctx, blob.Digest([]byte("never stored"))); err != nil {
		t.Errorf("Delete of an absent blob: %v, want nil", err)
	}

	if _, err := s.Get(ctx, stored.Digest); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("Get after Delete = %v, want ErrNotFound", err)
	}
}

func testDeleteIsNarrow(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	kept := put(t, s, "keep me\n")
	removed := put(t, s, "remove me\n")

	if err := s.Delete(ctx, removed.Digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if got := read(t, s, kept.Digest); got != "keep me\n" {
		t.Errorf("the surviving blob reads %q", got)
	}
}

// An empty patch is a real outcome — a pull that delivers nothing — and it has
// a digest like any other content.
func testEmptyContent(t *testing.T, newStore Factory) {
	s := newStore(t)

	descriptor, err := s.Put(context.Background(), strings.NewReader(""), "", 0)
	if err != nil {
		t.Fatalf("Put of empty content: %v", err)
	}
	if descriptor.Size != 0 {
		t.Errorf("Size = %d, want 0", descriptor.Size)
	}
	if descriptor.Digest != blob.Digest(nil) {
		t.Errorf("Digest = %s, want the hash of no bytes", descriptor.Digest)
	}
	if got := read(t, s, descriptor.Digest); got != "" {
		t.Errorf("read back %q, want empty", got)
	}
}

// Digests arrive from clients and are used to address storage. An
// implementation that passed one through unchecked would let a caller reach
// outside the store — a path on a filesystem, a key prefix in a bucket.
func testMalformedDigests(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	for _, digest := range []string{
		"",
		"sha256:",
		"sha256:tooshort",
		"sha256:../../../etc/passwd",
		"md5:d41d8cd98f00b204e9800998ecf8427e",
		strings.Repeat("a", 64),
		"sha256:" + strings.Repeat("Z", 64),
		"sha256:" + strings.Repeat("a", 63) + "/",
	} {
		t.Run(digest, func(t *testing.T) {
			if _, err := s.Get(ctx, digest); err == nil {
				t.Errorf("Get(%q) succeeded", digest)
			}
			if _, err := s.Stat(ctx, digest); err == nil {
				t.Errorf("Stat(%q) succeeded", digest)
			}
		})
	}
}

// Patches are megabytes, not kilobytes. An implementation that buffered badly
// or truncated at a chunk boundary passes every small test and fails on the
// first real push.
func testLargePayload(t *testing.T, newStore Factory) {
	s := newStore(t)

	content := bytes.Repeat([]byte("0123456789abcdef"), 400_000) // ~6 MiB

	descriptor, err := s.Put(context.Background(), bytes.NewReader(content), "", 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if descriptor.Size != int64(len(content)) {
		t.Fatalf("Size = %d, want %d", descriptor.Size, len(content))
	}

	r, err := s.Get(context.Background(), descriptor.Digest)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer r.Close()

	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	if !bytes.Equal(got, content) {
		t.Errorf("read back %d bytes that differ from the %d written", len(got), len(content))
	}
}

// Two workers uploading the same patch at the same time is ordinary: a retried
// push and its original, or two developers with the same change. Neither may
// observe the other's half-written blob.
func testConcurrentPuts(t *testing.T, newStore Factory) {
	s := newStore(t)
	ctx := context.Background()

	content := strings.Repeat("concurrent\n", 5000)
	want := blob.Digest([]byte(content))

	const writers = 8

	errs := make(chan error, writers)

	for range writers {
		go func() {
			descriptor, err := s.Put(ctx, strings.NewReader(content), want, 0)
			if err != nil {
				errs <- err
				return
			}
			if descriptor.Digest != want {
				errs <- errors.New("a concurrent Put returned the wrong digest")
				return
			}

			errs <- nil
		}()
	}

	for range writers {
		if err := <-errs; err != nil {
			t.Errorf("concurrent Put: %v", err)
		}
	}

	if got := read(t, s, want); got != content {
		t.Errorf("the blob is %d bytes after concurrent writes, want %d", len(got), len(content))
	}
}
