package pullcache_test

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/blob/filesystem"
	"github.com/NitScm/nit/internal/pullcache"
	"github.com/NitScm/nit/pkg/protocol"
	pullcachepkg "github.com/NitScm/nit/pkg/pullcache"
	"github.com/NitScm/nit/pkg/pullcache/pullcachetest"
)

// TestConformance runs the shared suite against the cache that ships.
//
// The tests beside this one cover what is specific to an LRU in one process —
// eviction order, a nil receiver, expiry against an injected clock. This covers
// what every implementation owes a caller, and it is the suite an out-of-tree
// one runs to earn the same trust.
func TestConformance(t *testing.T) {
	pullcachetest.Run(t, func(t *testing.T) (pullcachepkg.Store, func(*testing.T, string) *protocol.Blob) {
		blobs, err := filesystem.New(t.TempDir())
		if err != nil {
			t.Fatalf("filesystem.New: %v", err)
		}

		cache := pullcache.New(blobs, time.Hour, nil)

		// This implementation verifies an entry against the blob store before
		// returning it, so the descriptors the suite uses have to be real.
		return cache, func(t *testing.T, content string) *protocol.Blob {
			t.Helper()

			descriptor, err := blobs.Put(context.Background(), bytes.NewReader([]byte(content)), "", 0)
			if err != nil {
				t.Fatalf("Put: %v", err)
			}

			return &protocol.Blob{Digest: descriptor.Digest, Size: descriptor.Size}
		}
	})
}
