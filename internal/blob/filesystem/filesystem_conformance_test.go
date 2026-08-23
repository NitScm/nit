package filesystem_test

import (
	"testing"

	"github.com/NitScm/nit/internal/blob/filesystem"
	"github.com/NitScm/nit/pkg/blob"
	"github.com/NitScm/nit/pkg/blob/blobtest"
)

// TestConformance runs the shared blob suite against the filesystem store.
//
// The tests beside this one cover what is specific to a directory — temporary
// files, path traversal. This covers what every implementation owes a caller,
// and it is the suite an out-of-tree backend runs to earn the same trust.
func TestConformance(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) blob.Store {
		s, err := filesystem.New(t.TempDir())
		if err != nil {
			t.Fatalf("filesystem.New: %v", err)
		}

		return s
	})
}
