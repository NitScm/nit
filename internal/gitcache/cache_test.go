package gitcache_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NitScm/nit/internal/gitcache"
	"github.com/NitScm/nit/pkg/gitx"
)

func newCache(t *testing.T) (*gitcache.Cache, string) {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	dir := t.TempDir()

	c := gitcache.New(gitx.NewExecGit(), dir, nil)
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
