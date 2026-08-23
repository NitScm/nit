package gitcache_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NitScm/nit/internal/gitcache"
	"github.com/NitScm/nit/pkg/gitx"
)

func newCache(t *testing.T) (*gitcache.Cache, string) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()

	c := gitcache.New(gitx.NewExecGit(), dir, 0, nil)
	if err := c.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	return c, dir
}

// upstream builds a repository with one commit and returns its path and head.
func upstream(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()

	for _, args := range [][]string{
		{"init", "--initial-branch=main", "."},
		{"config", "user.email", "seed@example.com"},
		{"config", "user.name", "Seed"},
	} {
		run(t, dir, args...)
	}

	write(t, dir, "file.txt", "one\n")
	run(t, dir, "add", "--all")
	run(t, dir, "commit", "-m", "one")

	return dir, strings.TrimSpace(run(t, dir, "rev-parse", "HEAD"))
}

func run(t *testing.T, dir string, args ...string) string {
	t.Helper()

	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return string(out)
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// The point of the whole thing: the second checkout must not clone again.
func TestSecondCheckoutReusesTheMirror(t *testing.T) {
	c, workDir := newCache(t)
	ctx := context.Background()

	source, head := upstream(t)

	for i := range 2 {
		repo, release, err := c.Checkout(ctx, source, source, head)
		if err != nil {
			t.Fatalf("checkout %d: %v", i, err)
		}
		if repo == nil {
			t.Fatalf("checkout %d returned no repo", i)
		}
		release()
	}

	mirrors, err := os.ReadDir(filepath.Join(workDir, "mirrors"))
	if err != nil {
		t.Fatalf("read mirrors: %v", err)
	}
	if len(mirrors) != 1 {
		t.Errorf("%d mirrors on disk, want one shared", len(mirrors))
	}
}

// A commit pushed upstream after the mirror was built must be reachable: the
// fetch before each checkout is what makes the cache correct rather than
// merely fast.
func TestCheckoutSeesCommitsAddedSinceTheLastTask(t *testing.T) {
	c, _ := newCache(t)
	ctx := context.Background()

	source, head := upstream(t)

	_, release, err := c.Checkout(ctx, source, source, head)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	release()

	write(t, source, "file.txt", "two\n")
	run(t, source, "add", "--all")
	run(t, source, "commit", "-m", "two")

	moved := strings.TrimSpace(run(t, source, "rev-parse", "HEAD"))

	repo, release, err := c.Checkout(ctx, source, source, moved)
	if err != nil {
		t.Fatalf("second checkout: %v", err)
	}
	defer release()

	got, err := repo.ResolveRef(ctx, "HEAD")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got != moved {
		t.Errorf("HEAD = %s, want the commit added since the mirror was built (%s)", got, moved)
	}
}

// The failure this whole design exists to prevent: a task that died mid-apply
// must not leave anything the next task can inherit.
func TestNextCheckoutInheritsNothingFromAFailedTask(t *testing.T) {
	c, _ := newCache(t)
	ctx := context.Background()

	source, head := upstream(t)

	first, release, err := c.Checkout(ctx, source, source, head)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}

	dir := repoDir(t, first)
	write(t, dir, "half-applied.txt", "left behind\n")
	run(t, dir, "add", "--all")

	release()

	second, release, err := c.Checkout(ctx, source, source, head)
	if err != nil {
		t.Fatalf("second checkout: %v", err)
	}
	defer release()

	next := repoDir(t, second)

	if _, err := os.Stat(filepath.Join(next, "half-applied.txt")); !os.IsNotExist(err) {
		t.Error("the second task inherited a file from the first")
	}

	if status := run(t, next, "status", "--porcelain"); status != "" {
		t.Errorf("the second worktree is not clean:\n%s", status)
	}
}

// Two branches of one repository are two partitions and run concurrently. They
// share a mirror, and git serializes ref updates per repository rather than
// globally, so the cache has to hold a lock the callers do not know about.
func TestConcurrentCheckoutsOnOneRepository(t *testing.T) {
	c, _ := newCache(t)
	ctx := context.Background()

	source, head := upstream(t)

	const workers = 8

	var wg sync.WaitGroup

	for range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			repo, release, err := c.Checkout(ctx, source, source, head)
			if err != nil {
				t.Errorf("Checkout: %v", err)
				return
			}
			defer release()

			if _, err := repo.ResolveRef(ctx, "HEAD"); err != nil {
				t.Errorf("ResolveRef: %v", err)
			}
		}()
	}

	wg.Wait()
}

// A mirror can be corrupted by an interrupted pack write or a full disk.
// Rebuilding costs one clone; not rebuilding poisons every later task for that
// repository.
func TestCorruptMirrorIsRebuilt(t *testing.T) {
	c, workDir := newCache(t)
	ctx := context.Background()

	source, head := upstream(t)

	_, release, err := c.Checkout(ctx, source, source, head)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	release()

	mirrors, err := os.ReadDir(filepath.Join(workDir, "mirrors"))
	if err != nil || len(mirrors) != 1 {
		t.Fatalf("expected one mirror, got %v (%v)", len(mirrors), err)
	}

	// Destroy the object database while keeping the directory, which is what a
	// half-written pack looks like from the outside.
	objects := filepath.Join(workDir, "mirrors", mirrors[0].Name(), "objects")
	if err := os.RemoveAll(objects); err != nil {
		t.Fatalf("corrupt mirror: %v", err)
	}
	if err := os.WriteFile(objects, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("corrupt mirror: %v", err)
	}

	repo, release, err := c.Checkout(ctx, source, source, head)
	if err != nil {
		t.Fatalf("checkout after corruption: %v", err)
	}
	defer release()

	if got, err := repo.ResolveRef(ctx, "HEAD"); err != nil || got != head {
		t.Errorf("HEAD = %s (%v), want %s", got, err, head)
	}
}

// repoDir is the worktree's path on disk.
func repoDir(t *testing.T, repo gitx.Repo) string {
	t.Helper()

	return repo.Dir()
}

// budgeted returns a cache over an existing work directory with a disk budget,
// so a test can build mirrors first and decide what fits afterwards.
func budgeted(t *testing.T, workDir string, budget int64) *gitcache.Cache {
	t.Helper()

	c := gitcache.New(gitx.NewExecGit(), workDir, budget, nil)
	if err := c.Prepare(); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	return c
}

// mirrorPaths returns the mirror directories currently on disk.
func mirrorPaths(t *testing.T, workDir string) []string {
	t.Helper()

	entries, err := os.ReadDir(filepath.Join(workDir, "mirrors"))
	if err != nil {
		t.Fatalf("read mirrors: %v", err)
	}

	var out []string
	for _, e := range entries {
		out = append(out, filepath.Join(workDir, "mirrors", e.Name()))
	}

	sort.Strings(out)

	return out
}

// A mirror persists between tasks, so nothing returns its disk on its own.
// Past the budget the least recently used one goes.
func TestTheLeastRecentlyUsedMirrorIsEvicted(t *testing.T) {
	c, workDir := newCache(t)
	ctx := context.Background()

	old, oldHead := upstream(t)
	recent, recentHead := upstream(t)

	for _, r := range []struct {
		source string
		head   string
	}{{old, oldHead}, {recent, recentHead}} {
		_, release, err := c.Checkout(ctx, r.source, r.source, r.head)
		if err != nil {
			t.Fatalf("checkout %s: %v", r.source, err)
		}
		release()
	}

	paths := mirrorPaths(t, workDir)
	if len(paths) != 2 {
		t.Fatalf("%d mirrors on disk, want 2", len(paths))
	}

	// Ages set explicitly rather than relying on the order of two checkouts a
	// microsecond apart, which a coarse filesystem clock can report as equal.
	oldest := ageMirror(t, workDir, old, -48*time.Hour)
	newest := ageMirror(t, workDir, recent, -time.Hour)

	// A budget one byte under what the two occupy: exactly one has to go.
	budget := dirBytes(t, paths[0]) + dirBytes(t, paths[1]) - 1

	budgeted(t, workDir, budget).Sweep(ctx)

	if _, err := os.Stat(oldest); !os.IsNotExist(err) {
		t.Errorf("the least recently used mirror survived the sweep (%v)", err)
	}
	if _, err := os.Stat(newest); err != nil {
		t.Errorf("the most recently used mirror was evicted: %v", err)
	}
}

// Evicting a mirror whose worktree is live would delete the objects a running
// task is reading. The disk it would free is about to be freed anyway.
func TestAMirrorInUseIsNeverEvicted(t *testing.T) {
	c, workDir := newCache(t)
	ctx := context.Background()

	source, head := upstream(t)

	repo, release, err := c.Checkout(ctx, source, source, head)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	defer release()

	// A budget nothing can satisfy: without the in-use rule this mirror goes.
	budgeted(t, workDir, 1).Sweep(ctx)

	// The cache under test is the one holding the worktree, so sweep on it too.
	c.Sweep(ctx)

	if got, err := repo.ResolveRef(ctx, "HEAD"); err != nil || got != head {
		t.Errorf("the worktree of a running task stopped working after a sweep: %s, %v", got, err)
	}
}

// A budget of zero is the documented way to turn eviction off.
func TestZeroBudgetEvictsNothing(t *testing.T) {
	c, workDir := newCache(t)
	ctx := context.Background()

	source, head := upstream(t)

	_, release, err := c.Checkout(ctx, source, source, head)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}
	release()

	c.Sweep(ctx)

	if got := len(mirrorPaths(t, workDir)); got != 1 {
		t.Errorf("%d mirrors after a sweep with no budget, want the one that was there", got)
	}
}

// An evicted repository is slow on its next task, not broken.
func TestAnEvictedRepositoryIsRebuiltOnItsNextCheckout(t *testing.T) {
	c, workDir := newCache(t)
	ctx := context.Background()

	source, head := upstream(t)

	_, release, err := c.Checkout(ctx, source, source, head)
	if err != nil {
		t.Fatalf("first checkout: %v", err)
	}
	release()

	budgeted(t, workDir, 1).Sweep(ctx)

	if got := len(mirrorPaths(t, workDir)); got != 0 {
		t.Fatalf("%d mirrors after a sweep that should have emptied the cache", got)
	}

	repo, release, err := c.Checkout(ctx, source, source, head)
	if err != nil {
		t.Fatalf("checkout after eviction: %v", err)
	}
	defer release()

	if got, err := repo.ResolveRef(ctx, "HEAD"); err != nil || got != head {
		t.Errorf("HEAD = %s (%v), want %s", got, err, head)
	}
}

// ageMirror backdates a mirror's last-used marker and returns its directory.
func ageMirror(t *testing.T, workDir, identity string, age time.Duration) string {
	t.Helper()

	sum := sha256.Sum256([]byte(identity))
	dir := filepath.Join(workDir, "mirrors", hex.EncodeToString(sum[:8])+".git")

	marker := filepath.Join(dir, "nit-last-used")
	when := time.Now().Add(age)

	if err := os.Chtimes(marker, when, when); err != nil {
		t.Fatalf("backdate %s: %v", marker, err)
	}

	return dir
}

func dirBytes(t *testing.T, dir string) int64 {
	t.Helper()

	var total int64

	err := filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}

		total += info.Size()

		return nil
	})
	if err != nil {
		t.Fatalf("size of %s: %v", dir, err)
	}

	return total
}
