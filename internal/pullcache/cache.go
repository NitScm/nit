// Package pullcache holds the pull projection cache that ships with nit.
//
// It is per process, and that is the whole design rather than a limitation. A
// release-day herd arrives within minutes on the same handful of workers, so a
// cache that spans one process already collapses it — without a table, a
// migration on three backends, or an invalidation story. A deployment that
// wants one cache across a fleet implements pullcache.Store instead.
//
// The contract, and the reasoning about what may be shared with whom, is in
// pkg/pullcache.
package pullcache

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/NitScm/nit/pkg/blob"
	"github.com/NitScm/nit/pkg/pullcache"
)

// Cache maps a projection key to the artifact that holds it.
type Cache struct {
	blobs blob.Store
	ttl   time.Duration
	max   int
	now   func() time.Time

	mu      sync.Mutex
	entries map[string]*list.Element
	order   *list.List // front is most recently used
}

type record struct {
	key     string
	entry   pullcache.Entry
	expires time.Time
}

// DefaultEntries is how many projections are remembered.
//
// Not configurable, because there is nothing to tune: an entry is a descriptor
// and a few counters, so a thousand of them cost less than one patch. The
// number that matters is distinct rights profiles per repository, and a policy
// with a thousand of those has a readability problem long before it has a cache
// problem.
const DefaultEntries = 1024

// New returns a cache over blobs.
//
// ttl must be comfortably shorter than the artifact TTL: an entry that outlives
// the patch it names turns a hit into a fetch failure. Get checks anyway, so
// this is defence in depth rather than the only guard.
func New(blobs blob.Store, ttl time.Duration, now func() time.Time) *Cache {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}

	return &Cache{
		blobs:   blobs,
		ttl:     ttl,
		max:     DefaultEntries,
		now:     now,
		entries: map[string]*list.Element{},
		order:   list.New(),
	}
}

// Get returns a cached projection, if one is present, unexpired, and still
// backed by bytes the client can fetch.
func (c *Cache) Get(ctx context.Context, key pullcache.Key) (pullcache.Entry, bool, error) {
	if c == nil || c.ttl <= 0 {
		return pullcache.Entry{}, false, nil
	}

	hashed := key.Hash()

	c.mu.Lock()

	element, ok := c.entries[hashed]
	if !ok {
		c.mu.Unlock()
		return pullcache.Entry{}, false, nil
	}

	held := element.Value.(*record)

	if !c.now().Before(held.expires) {
		c.removeLocked(element)
		c.mu.Unlock()

		return pullcache.Entry{}, false, nil
	}

	c.order.MoveToFront(element)
	entry := held.entry

	c.mu.Unlock()

	// Outside the lock: a Stat can touch a filesystem or a network, and holding
	// the cache shut for it would serialize every pull in the process.
	if entry.Patch != nil {
		if _, err := c.blobs.Stat(ctx, entry.Patch.Digest); err != nil {
			// Swept, or unreachable. Either way this entry is no longer an
			// answer. Dropping it is right in both cases: a transient failure
			// costs one recomputation, a permanent one would otherwise be
			// served until the entry expired.
			c.mu.Lock()
			if element, ok := c.entries[hashed]; ok {
				c.removeLocked(element)
			}
			c.mu.Unlock()

			return pullcache.Entry{}, false, nil
		}
	}

	return entry, true, nil
}

// Put records a projection.
func (c *Cache) Put(_ context.Context, key pullcache.Key, entry pullcache.Entry) error {
	if c == nil || c.ttl <= 0 {
		return nil
	}

	hashed := key.Hash()

	c.mu.Lock()
	defer c.mu.Unlock()

	if element, ok := c.entries[hashed]; ok {
		held := element.Value.(*record)
		held.entry = entry
		held.expires = c.now().Add(c.ttl)

		c.order.MoveToFront(element)

		return nil
	}

	element := c.order.PushFront(&record{
		key:     hashed,
		entry:   entry,
		expires: c.now().Add(c.ttl),
	})
	c.entries[hashed] = element

	for c.order.Len() > c.max {
		c.removeLocked(c.order.Back())
	}

	return nil
}

// Len reports how many entries are held, for tests and diagnostics.
func (c *Cache) Len() int {
	if c == nil {
		return 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	return c.order.Len()
}

func (c *Cache) removeLocked(element *list.Element) {
	if element == nil {
		return
	}

	delete(c.entries, element.Value.(*record).key)
	c.order.Remove(element)
}

var _ pullcache.Store = (*Cache)(nil)
