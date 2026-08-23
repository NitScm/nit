package nitd

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NitScm/nit/internal/blob/filesystem"
	"github.com/NitScm/nit/pkg/policy"
)

// The layout is part of the contract with an operator: they mount a volume at
// storage.blob_dir and expect to find what nit wrote under it.
func TestBlobsAreWrittenUnderTheirTenant(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	store, err := tenantBlobs(root, policy.DefaultTenant)
	if err != nil {
		t.Fatalf("tenantBlobs: %v", err)
	}

	descriptor, err := store.Put(ctx, strings.NewReader("a patch\n"), "", 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	scoped := filepath.Join(root, string(policy.DefaultTenant))

	if !hasFiles(t, scoped) {
		t.Errorf("nothing was written under %s", scoped)
	}

	// And the blob is readable through the store that wrote it.
	if _, err := store.Stat(ctx, descriptor.Digest); err != nil {
		t.Errorf("Stat: %v", err)
	}
}

// An upgrade must not lose the patches already in flight. A push interrupted by
// a restart would otherwise fail with missing_patch — the error operators
// already associate with a misconfigured deployment, handed to them by an
// upgrade that changed a path.
func TestABlobFromBeforeTheMoveIsStillReadable(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()

	// Written the way the previous version wrote it: straight into the root.
	legacy, err := filesystem.New(root)
	if err != nil {
		t.Fatalf("filesystem.New: %v", err)
	}

	descriptor, err := legacy.Put(ctx, strings.NewReader("uploaded before the upgrade\n"), "", 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	store, err := tenantBlobs(root, policy.DefaultTenant)
	if err != nil {
		t.Fatalf("tenantBlobs: %v", err)
	}

	body, err := store.Get(ctx, descriptor.Digest)
	if err != nil {
		t.Fatalf("a blob written before the move was lost: %v", err)
	}
	body.Close()
}

func hasFiles(t *testing.T, dir string) bool {
	t.Helper()

	found := false

	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			found = true
		}
		return nil
	})
	if err != nil {
		return false
	}

	return found
}
