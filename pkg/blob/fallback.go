package blob

import (
	"context"
	"errors"
	"io"
)

// Fallback reads from a second store when the first does not have a blob.
//
// It exists for one job: moving a blob namespace without losing what is already
// in it. Writes go to Primary alone, so the old location stops growing the
// moment this is installed; reads try Primary and fall back, so a patch
// uploaded before the move is still fetchable after it.
//
// # Why that matters more than it sounds
//
// The blob store holds the authorized patch between the control plane and the
// worker. A patch that becomes unreachable mid-flight is a push that fails with
// missing_patch — the error operators already recognise as the most common way
// to get a deployment wrong. Handing them that error *because of an upgrade*
// would teach exactly the wrong lesson.
//
// # It is transitional, and should be removed
//
// Once no blob predates the move — one artifact TTL, a day by default — the
// fallback is dead weight that keeps a directory alive for no reason. Remove
// it, and the old location can be deleted.
type Fallback struct {
	// Primary receives every write and answers first.
	Primary Store

	// Secondary is consulted only when Primary reports ErrNotFound.
	Secondary Store
}

// Put writes to the primary only.
//
// Never to both: two copies of the same bytes under the same digest are not a
// safety net, they are twice the disk and a second thing to delete.
func (f Fallback) Put(ctx context.Context, r io.Reader, expected string, maxSize int64) (Descriptor, error) {
	return f.Primary.Put(ctx, r, expected, maxSize)
}

func (f Fallback) Get(ctx context.Context, digest string) (io.ReadCloser, error) {
	body, err := f.Primary.Get(ctx, digest)
	if !errors.Is(err, ErrNotFound) {
		return body, err
	}

	return f.Secondary.Get(ctx, digest)
}

func (f Fallback) Stat(ctx context.Context, digest string) (Descriptor, error) {
	descriptor, err := f.Primary.Stat(ctx, digest)
	if !errors.Is(err, ErrNotFound) {
		return descriptor, err
	}

	return f.Secondary.Stat(ctx, digest)
}

// Delete removes the blob from both.
//
// Both, because artifact collection is what eventually empties the old
// location: a delete that only reached the primary would leave the secondary
// growing forever, which is the opposite of what a transition is for.
func (f Fallback) Delete(ctx context.Context, digest string) error {
	if err := f.Primary.Delete(ctx, digest); err != nil {
		return err
	}

	return f.Secondary.Delete(ctx, digest)
}

var _ Store = Fallback{}
