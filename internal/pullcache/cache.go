// Package pullcache shares a filtered projection between users whose read
// rights are identical.
//
// A pull clones, diffs sync_point..tip, and filters the result per user. Five
// hundred developers pulling after a release is five hundred diffs and five
// hundred filter passes for one upstream change — the cost that arrives on a
// release day rather than gradually.
//
// The observation that removes it: the output depends on the subject only
// through what the subject may read. Everyone in the same groups, with the same
// exemptions, at the same ref, under the same bundle, gets the same bytes. The
// first pull after a release can therefore serve all the others.
//
// # What is cached, and what is not
//
// The entry holds a *descriptor* — the digest, size and file counts — not the
// patch. The bytes already live in the blob store under that digest, so an
// entry is a few dozen bytes and the cache cannot grow into a memory problem
// however many profiles a policy has.
//
// It is per process and deliberately not shared between workers. A shared cache
// would need a table, a migration on three backends, and an invalidation story;
// a per-worker one needs none of that and still collapses a release-day herd,
// because the herd arrives within minutes on the same handful of workers. The
// shared version is a later decision, not a missing piece of this one.
//
// # Why an entry cannot outlive what it points at
//
// A generated pull patch is an artifact with a TTL, and something eventually
// collects it. An entry naming a digest that has been swept would hand a client
// a patch it cannot fetch. Two things prevent that: entries expire well inside
// the artifact TTL, and Get verifies the blob is still there before returning a
// hit. A missing blob is a miss, not an error — the caller recomputes, which is
// exactly what it would have done anyway.
package pullcache

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"sync"
	"time"

	"github.com/NitScm/nit/pkg/blob"
	"github.com/NitScm/nit/pkg/protocol"
)

// Entry is what a hit returns: everything a pull result needs except the bytes.
type Entry struct {
	// Patch is nil when the projection was empty — a real outcome worth
	// caching, since a user who may read nothing that changed asks repeatedly
	// and gets nothing repeatedly.
	Patch *protocol.Blob

	FilesTotal     int
	FilesDelivered int
	FilesWithheld  int
}

// Key identifies one filtered projection.
//
// Repository, From and To fix the diff; Profile fixes what may be read of it.
// The profile already carries the policy version and the ref (see
// policy.Profile), so those are not repeated here — repeating them would invite
// the belief that they are what makes the key safe, when the profile is.
type Key struct {
	Repository string
	From       string
	To         string
	Profile    string
}

func (k Key) hash() string {
	h := sha256.New()

	for _, part := range []string{k.Repository, k.From, k.To, k.Profile} {
		h.Write([]byte(strconv.Itoa(len(part))))
		h.Write([]byte(":"))
		h.Write([]byte(part))
	}

	return hex.EncodeToString(h.Sum(nil))
}

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
	entry   Entry
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
func (c *Cache) Get(ctx context.Context, key Key) (Entry, bool) {
	if c == nil || c.ttl <= 0 {
		return Entry{}, false
	}

	hashed := key.hash()

	c.mu.Lock()

	element, ok := c.entries[hashed]
	if !ok {
		c.mu.Unlock()
		return Entry{}, false
	}

	held := element.Value.(*record)

	if !c.now().Before(held.expires) {
		c.removeLocked(element)
		c.mu.Unlock()

		return Entry{}, false
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

			return Entry{}, false
		}
	}

	return entry, true
}

// Put records a projection.
func (c *Cache) Put(key Key, entry Entry) {
	if c == nil || c.ttl <= 0 {
		return
	}

	hashed := key.hash()

	c.mu.Lock()
	defer c.mu.Unlock()

	if element, ok := c.entries[hashed]; ok {
		held := element.Value.(*record)
		held.entry = entry
		held.expires = c.now().Add(c.ttl)

		c.order.MoveToFront(element)

		return
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
