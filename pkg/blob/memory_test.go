package blob_test

import (
	"testing"

	"github.com/NitScm/nit/pkg/blob"
	"github.com/NitScm/nit/pkg/blob/blobtest"
)

// The point of shipping this rather than letting each consumer write their own
// forty lines: it is held to the same contract as the store that ships, so a
// test that passes against it means something.
func TestMemoryConformance(t *testing.T) {
	blobtest.Run(t, func(t *testing.T) blob.Store {
		return blob.NewMemory()
	})
}
