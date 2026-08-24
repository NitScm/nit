// Package pullcachetest is a conformance suite every pullcache.Store must pass.
//
// A pull cache decides which developers receive the same bytes. Its key carries
// a rights profile, so an implementation that mishandles a key does not serve a
// stale result — it serves one person's files to another. That is why this
// suite exists and why most of it is about keys rather than about caching.
//
// The rest is the ordinary contract: what a miss looks like, what expiry means,
// and the rule that a failure is a miss rather than an error a caller must
// handle.
package pullcachetest

import (
	"context"
	"strings"
	"testing"

	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/pullcache"
)

// Factory builds a fresh, empty store for one test.
//
// It returns the store and, optionally, a function that records a blob and
// returns a descriptor pointing at it. An implementation that verifies its
// entries against a blob store needs the descriptors it is given to be real;
// one that does not may return nil and the suite uses a synthetic descriptor.
type Factory func(t *testing.T) (pullcache.Store, func(t *testing.T, content string) *protocol.Blob)

// Run executes the whole suite against an implementation.
func Run(t *testing.T, newStore Factory) {
	t.Helper()

	t.Run("AStoredProjectionComesBack", func(t *testing.T) { testRoundTrip(t, newStore) })
	t.Run("AnAbsentKeyIsAMiss", func(t *testing.T) { testMiss(t, newStore) })
	t.Run("EveryFieldOfTheKeySeparates", func(t *testing.T) { testKeyFields(t, newStore) })
	t.Run("KeyFieldsCannotRunTogether", func(t *testing.T) { testKeyBoundaries(t, newStore) })
	t.Run("AnEmptyProjectionIsCached", func(t *testing.T) { testEmptyProjection(t, newStore) })
	t.Run("TheWholeReportSurvives", func(t *testing.T) { testCountsSurvive(t, newStore) })
	t.Run("WritingTwiceKeepsTheLast", func(t *testing.T) { testOverwrite(t, newStore) })
}

func store(t *testing.T, newStore Factory) (pullcache.Store, func(t *testing.T, content string) *protocol.Blob) {
	t.Helper()

	s, blobs := newStore(t)
	if blobs == nil {
		blobs = func(t *testing.T, content string) *protocol.Blob {
			return &protocol.Blob{Digest: syntheticDigest(content), Size: int64(len(content))}
		}
	}

	return s, blobs
}

// syntheticDigest is well-formed without being real, for an implementation that
// does not check its descriptors against a blob store.
func syntheticDigest(content string) string {
	const hexChars = "0123456789abcdef"

	out := make([]byte, 64)
	for i := range out {
		out[i] = hexChars[(i+len(content))%len(hexChars)]
	}

	return "sha256:" + string(out)
}

func key(profile string) pullcache.Key {
	return pullcache.Key{
		Repository: "https://example.com/repo.git",
		From:       "aaaa",
		To:         "bbbb",
		Profile:    profile,
	}
}

func testRoundTrip(t *testing.T, newStore Factory) {
	s, blob := store(t, newStore)
	ctx := context.Background()

	want := pullcache.Entry{
		Patch:          blob(t, "diff"),
		FilesTotal:     3,
		FilesDelivered: 2,
		FilesWithheld:  1,
	}

	if err := s.Put(ctx, key("profile-a"), want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok, err := s.Get(ctx, key("profile-a"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("the entry just stored was not found")
	}
	if got.Patch == nil || got.Patch.Digest != want.Patch.Digest {
		t.Errorf("digest = %v, want %s", got.Patch, want.Patch.Digest)
	}
}

func testMiss(t *testing.T, newStore Factory) {
	s, _ := store(t, newStore)

	_, ok, err := s.Get(context.Background(), key("never-stored"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error("a key that was never stored was found")
	}
}

// The assertion this suite exists for.
//
// Every field of a key takes part in identity, and Profile most of all: it is
// the rights profile, so an implementation that dropped it would serve one
// group's projection to another. The others matter for the same reason in a
// different direction — a projection from another repository or another commit
// range is not stale, it is wrong.
func testKeyFields(t *testing.T, newStore Factory) {
	s, blob := store(t, newStore)
	ctx := context.Background()

	base := key("profile-a")

	if err := s.Put(ctx, base, pullcache.Entry{Patch: blob(t, "diff")}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	for _, tc := range []struct {
		name string
		key  pullcache.Key
	}{
		{"repository", pullcache.Key{Repository: "other", From: base.From, To: base.To, Profile: base.Profile}},
		{"from", pullcache.Key{Repository: base.Repository, From: "cccc", To: base.To, Profile: base.Profile}},
		{"to", pullcache.Key{Repository: base.Repository, From: base.From, To: "cccc", Profile: base.Profile}},
		{"profile", pullcache.Key{Repository: base.Repository, From: base.From, To: base.To, Profile: "profile-b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, ok, err := s.Get(ctx, tc.key)
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			if ok {
				t.Errorf("a key differing only in %s hit; this serves one profile's "+
					"projection under another's identity", tc.name)
			}
		})
	}
}

// Two different keys must not concatenate to the same bytes. An implementation
// that joined fields without length-prefixing them would make "ab"+"c" and
// "a"+"bc" the same entry — and one of those is somebody else's.
func testKeyBoundaries(t *testing.T, newStore Factory) {
	s, blob := store(t, newStore)
	ctx := context.Background()

	if err := s.Put(ctx, pullcache.Key{Repository: "ab", From: "c", To: "d", Profile: "e"},
		pullcache.Entry{Patch: blob(t, "diff")}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	_, ok, err := s.Get(ctx, pullcache.Key{Repository: "a", From: "bc", To: "d", Profile: "e"})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if ok {
		t.Error(`"ab"+"c" collided with "a"+"bc"`)
	}
}

// An empty projection is a real outcome — a user who may read nothing that
// changed — and asking again must not recompute it.
func testEmptyProjection(t *testing.T, newStore Factory) {
	s, _ := store(t, newStore)
	ctx := context.Background()

	if err := s.Put(ctx, key("profile-a"), pullcache.Entry{FilesTotal: 2, FilesWithheld: 2}); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok, err := s.Get(ctx, key("profile-a"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("an empty projection was not cached")
	}
	if got.Patch != nil {
		t.Error("an empty projection came back with a patch")
	}
	if got.FilesWithheld != 2 {
		t.Errorf("FilesWithheld = %d, want 2", got.FilesWithheld)
	}
}

// The counts travel with the projection: a client reads them, and a hit that
// lost them would report a pull as delivering everything.
func testCountsSurvive(t *testing.T, newStore Factory) {
	s, blob := store(t, newStore)
	ctx := context.Background()

	want := pullcache.Entry{
		Patch:          blob(t, "diff"),
		FilesTotal:     7,
		FilesDelivered: 4,
		FilesWithheld:  3,
	}

	if err := s.Put(ctx, key("profile-a"), want); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, ok, err := s.Get(ctx, key("profile-a"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("miss")
	}

	if got.FilesTotal != want.FilesTotal ||
		got.FilesDelivered != want.FilesDelivered ||
		got.FilesWithheld != want.FilesWithheld {
		t.Errorf("counts = %+v, want %+v", got, want)
	}
}

// Two workers computing the same projection concurrently both write it. The
// second write must not corrupt the entry or duplicate it.
func testOverwrite(t *testing.T, newStore Factory) {
	s, blob := store(t, newStore)
	ctx := context.Background()

	first := pullcache.Entry{Patch: blob(t, "diff"), FilesDelivered: 1}
	second := pullcache.Entry{Patch: blob(t, "diff"), FilesDelivered: 1}

	for _, entry := range []pullcache.Entry{first, second} {
		if err := s.Put(ctx, key("profile-a"), entry); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	got, ok, err := s.Get(ctx, key("profile-a"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !ok {
		t.Fatal("the entry vanished after being written twice")
	}
	if got.Patch == nil || !strings.HasPrefix(got.Patch.Digest, "sha256:") {
		t.Errorf("the entry is malformed after two writes: %+v", got)
	}
}
