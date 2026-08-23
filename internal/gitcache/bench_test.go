package gitcache_test

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// Compares what a task pays with the cache against a full clone.
//
// It is a signal, not a benchmark, and it is worth reading the caveat: the
// first version of this measured a *local* clone and reported no gain at all,
// because git hardlinks the object database when the source is a path on the
// same filesystem. That measures nothing a deployment would experience. Using
// file:// forces the same pack-and-transfer a network remote does, and the
// difference appears.
//
// The real figure depends on repository size and network, neither of which
// this can stand in for. What it does prove is that the cached path does less
// work, and by roughly the shape one would expect.
func TestCloneVersusCachedCheckout(t *testing.T) {
	c, _ := newCache(t)
	ctx := context.Background()

	source, head := upstream(t)

	// 400 commits: small next to a real monorepo, big enough that a clone is
	// not free.
	for i := range 400 {
		write(t, source, "file.txt", string(rune('a'+i%26))+"\n")
		run(t, source, "add", "--all")
		run(t, source, "commit", "-m", "commit")
	}
	head = commitAt(t, source)

	// Warm the mirror, as a second task on the same repository would find it.
	_, release, err := c.Checkout(ctx, "file://"+source, "file://"+source, head)
	if err != nil {
		t.Fatalf("warm: %v", err)
	}
	release()

	// file:// rather than a bare path: a local clone hardlinks the object
	// database, which makes it almost free and measures nothing a real
	// deployment would experience. The file transport packs and transfers as a
	// network remote does.
	remote := "file://" + source

	cached := timeIt(t, func() {
		_, release, err := c.Checkout(ctx, remote, remote, head)
		if err != nil {
			t.Fatalf("cached: %v", err)
		}
		release()
	})

	fresh := timeIt(t, func() {
		dir := t.TempDir() + "/clone"
		cmd := exec.Command("git", "clone", "--quiet", "--branch", "main", remote, dir)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("clone: %v\n%s", err, out)
		}
	})

	t.Logf("full clone      %v", fresh)
	t.Logf("cached checkout %v", cached)
	t.Logf("ratio           %.1fx", float64(fresh)/float64(cached))
}

func commitAt(t *testing.T, dir string) string {
	t.Helper()
	return strings.TrimSpace(run(t, dir, "rev-parse", "HEAD"))
}

func timeIt(t *testing.T, f func()) time.Duration {
	t.Helper()
	start := time.Now()
	f()
	return time.Since(start)
}
