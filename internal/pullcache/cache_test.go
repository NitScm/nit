package pullcache_test

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/blob/filesystem"
	"github.com/NitScm/nit/internal/pullcache"
	"github.com/NitScm/nit/pkg/blob"
	"github.com/NitScm/nit/pkg/protocol"
)

func newCache(t *testing.T, ttl time.Duration, now func() time.Time) (*pullcache.Cache, blob.Store) {
	t.Helper()

	blobs, err := filesystem.New(t.TempDir())
	if err != nil {
		t.Fatalf("filesystem.New: %v", err)
	}

	return pullcache.New(blobs, ttl, now), blobs
}

// store writes bytes and returns the descriptor a cache entry would carry.
func store(t *testing.T, blobs blob.Store, content string) *protocol.Blob {
	t.Helper()

	descriptor, err := blobs.Put(context.Background(), bytes.NewReader([]byte(content)), "", 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	return &protocol.Blob{Digest: descriptor.Digest, Size: descriptor.Size}
}

func key(profile string) pullcache.Key {
	return pullcache.Key{
		Repository: "https://example.com/repo.git",
		From:       "aaaa",
		To:         "bbbb",
		Profile:    profile,
	}
}

func TestAStoredProjectionComesBack(t *testing.T) {
	c, blobs := newCache(t, time.Hour, nil)
	ctx := context.Background()

	want := pullcache.Entry{
		Patch:          store(t, blobs, "diff"),
		FilesTotal:     3,
		FilesDelivered: 2,
		FilesWithheld:  1,
	}

	c.Put(key("profile-a"), want)

	got, ok := c.Get(ctx, key("profile-a"))
	if !ok {
		t.Fatal("the entry just stored was not found")
	}
	if got.Patch.Digest != want.Patch.Digest {
		t.Errorf("digest = %s, want %s", got.Patch.Digest, want.Patch.Digest)
	}
	if got.FilesWithheld != 1 || got.FilesDelivered != 2 || got.FilesTotal != 3 {
		t.Errorf("counts = %+v, want the ones stored", got)
	}
}

// Every field of the key separates. A key that ignored one would serve a
// projection computed for a different repository, range or rights profile.
func TestEveryPartOfTheKeySeparates(t *testing.T) {
	c, blobs := newCache(t, time.Hour, nil)
	ctx := context.Background()

	base := key("profile-a")
	c.Put(base, pullcache.Entry{Patch: store(t, blobs, "diff")})

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
			if _, ok := c.Get(ctx, tc.key); ok {
				t.Errorf("a key differing only in %s hit", tc.name)
			}
		})
	}
}

// Field boundaries are hashed, so no two different keys can concatenate to the
// same bytes.
func TestKeyFieldsCannotRunTogether(t *testing.T) {
	c, blobs := newCache(t, time.Hour, nil)
	ctx := context.Background()

	c.Put(pullcache.Key{Repository: "ab", From: "c", To: "d", Profile: "e"},
		pullcache.Entry{Patch: store(t, blobs, "diff")})

	if _, ok := c.Get(ctx, pullcache.Key{Repository: "a", From: "bc", To: "d", Profile: "e"}); ok {
		t.Error(`"ab"+"c" collided with "a"+"bc"`)
	}
}

func TestAnExpiredEntryIsAMiss(t *testing.T) {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }

	c, blobs := newCache(t, time.Minute, clock)
	ctx := context.Background()

	c.Put(key("profile-a"), pullcache.Entry{Patch: store(t, blobs, "diff")})

	now = now.Add(59 * time.Second)
	if _, ok := c.Get(ctx, key("profile-a")); !ok {
		t.Error("the entry expired early")
	}

	now = now.Add(2 * time.Second)
	if _, ok := c.Get(ctx, key("profile-a")); ok {
		t.Error("the entry outlived its TTL")
	}
	if c.Len() != 0 {
		t.Errorf("%d entries remain; an expired one should be dropped when found", c.Len())
	}
}

// The failure the design exists to avoid: an entry naming a patch that has been
// swept. A client handed that digest cannot fetch it, and the pull fails for a
// reason nobody can act on.
func TestAnEntryWhoseBlobIsGoneIsAMiss(t *testing.T) {
	c, blobs := newCache(t, time.Hour, nil)
	ctx := context.Background()

	descriptor := store(t, blobs, "diff")
	c.Put(key("profile-a"), pullcache.Entry{Patch: descriptor})

	if err := blobs.Delete(ctx, descriptor.Digest); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	if _, ok := c.Get(ctx, key("profile-a")); ok {
		t.Fatal("a hit named a patch that no longer exists")
	}

	// And it is dropped rather than checked again on every pull.
	if c.Len() != 0 {
		t.Errorf("%d entries remain after a dangling one was found", c.Len())
	}
}

// An empty projection is a real answer — a user who may read nothing that
// changed — and asking again should not recompute it.
func TestAnEmptyProjectionIsCached(t *testing.T) {
	c, _ := newCache(t, time.Hour, nil)
	ctx := context.Background()

	c.Put(key("profile-a"), pullcache.Entry{FilesTotal: 2, FilesWithheld: 2})

	got, ok := c.Get(ctx, key("profile-a"))
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

func TestTheLeastRecentlyUsedEntryIsEvicted(t *testing.T) {
	c, blobs := newCache(t, time.Hour, nil)
	ctx := context.Background()

	descriptor := store(t, blobs, "diff")

	// One more than the cache holds.
	for i := range pullcache.DefaultEntries + 1 {
		c.Put(key(fmt.Sprintf("profile-%d", i)), pullcache.Entry{Patch: descriptor})
	}

	if c.Len() != pullcache.DefaultEntries {
		t.Errorf("Len = %d, want the cache bounded at %d", c.Len(), pullcache.DefaultEntries)
	}
	if _, ok := c.Get(ctx, key("profile-0")); ok {
		t.Error("the oldest entry survived; the cache is not bounded by use")
	}
	if _, ok := c.Get(ctx, key(fmt.Sprintf("profile-%d", pullcache.DefaultEntries))); !ok {
		t.Error("the newest entry was evicted")
	}
}

// Reading an entry keeps it, which is what makes eviction least-recently-*used*
// rather than oldest-first: a profile pulled by half the company should not be
// dropped for one pulled once.
func TestAReadEntrySurvivesEviction(t *testing.T) {
	c, blobs := newCache(t, time.Hour, nil)
	ctx := context.Background()

	descriptor := store(t, blobs, "diff")

	for i := range pullcache.DefaultEntries {
		c.Put(key(fmt.Sprintf("profile-%d", i)), pullcache.Entry{Patch: descriptor})
	}

	// Touch the oldest, then push one more in.
	if _, ok := c.Get(ctx, key("profile-0")); !ok {
		t.Fatal("the oldest entry was already gone")
	}

	c.Put(key("profile-new"), pullcache.Entry{Patch: descriptor})

	if _, ok := c.Get(ctx, key("profile-0")); !ok {
		t.Error("a recently read entry was evicted")
	}
	if _, ok := c.Get(ctx, key("profile-1")); ok {
		t.Error("the actual least recently used entry survived")
	}
}

// A zero TTL turns the cache off. Nothing is stored, so nothing can be served.
func TestAZeroTTLDisablesTheCache(t *testing.T) {
	c, blobs := newCache(t, 0, nil)
	ctx := context.Background()

	c.Put(key("profile-a"), pullcache.Entry{Patch: store(t, blobs, "diff")})

	if _, ok := c.Get(ctx, key("profile-a")); ok {
		t.Error("a cache with no TTL served an entry")
	}
	if c.Len() != 0 {
		t.Errorf("Len = %d, want nothing stored", c.Len())
	}
}

// A nil cache behaves as a disabled one, so a caller that never built one does
// not have to guard every call.
func TestANilCacheIsUsable(t *testing.T) {
	var c *pullcache.Cache

	if _, ok := c.Get(context.Background(), key("profile-a")); ok {
		t.Error("a nil cache returned a hit")
	}

	c.Put(key("profile-a"), pullcache.Entry{})

	if c.Len() != 0 {
		t.Error("a nil cache reported entries")
	}
}

func TestConcurrentUse(t *testing.T) {
	c, blobs := newCache(t, time.Hour, nil)
	ctx := context.Background()

	descriptor := store(t, blobs, "diff")

	done := make(chan struct{})

	for worker := range 8 {
		go func() {
			defer func() { done <- struct{}{} }()

			for i := range 200 {
				k := key(fmt.Sprintf("profile-%d", (worker*200+i)%50))

				if _, ok := c.Get(ctx, k); !ok {
					c.Put(k, pullcache.Entry{Patch: descriptor})
				}
			}
		}()
	}

	for range 8 {
		<-done
	}

	if c.Len() == 0 || c.Len() > 50 {
		t.Errorf("Len = %d, want between 1 and the 50 distinct profiles", c.Len())
	}
}
