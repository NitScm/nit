// Package gitcache keeps a bare mirror per repository so a task pays for the
// delta rather than for a whole clone.
//
// Every task used to clone from scratch. On a large repository that dominates
// every other cost, and because pushes to one branch are serialized, the clone
// time *is* that branch's throughput — which is what put a big monorepo at a
// dozen pushes an hour.
//
// The shape is a shared mirror and a private worktree:
//
//	work_dir/mirrors/<key>.git    fetched before each use, shared
//	work_dir/tasks/<task>/        created and destroyed per task
//
// A worktree is never reused. A task that died mid-apply leaves a dirty one,
// and the failure mode of inheriting it is not a broken build — it is a wrong
// commit landing on the forge under a developer's name. Cheap isolation is
// worth more than a saved checkout.
package gitcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/NitScm/nit/pkg/gitx"
)

// Cache hands out worktrees backed by shared mirrors.
type Cache struct {
	git     gitx.Git
	workDir string
	log     *slog.Logger

	// budget caps the disk mirrors may occupy. Zero means no eviction.
	budget int64

	// lastSweep rate-limits the sweep. Measuring the mirrors means walking
	// them, and a mirror of a large repository is a lot of files to stat after
	// every single task.
	lastSweep time.Time

	// mu guards mirrors and inUse; each mirror has its own lock because git
	// serializes ref updates per repository, not globally: two tasks fetching
	// into one mirror race on ref locks, two tasks on different mirrors do not.
	mu      sync.Mutex
	mirrors map[string]*sync.Mutex

	// inUse counts the worktrees currently cut from each mirror. A mirror with
	// a live worktree is never evicted: removing it would pull the ground from
	// under a running task.
	inUse map[string]int
}

// New returns a cache rooted at workDir.
//
// budget caps the disk mirrors may occupy; zero disables eviction, which is
// only appropriate when something else is watching the volume.
func New(git gitx.Git, workDir string, budget int64, log *slog.Logger) *Cache {
	if log == nil {
		log = slog.Default()
	}

	return &Cache{
		git:     git,
		workDir: workDir,
		budget:  budget,
		log:     log,
		mirrors: map[string]*sync.Mutex{},
		inUse:   map[string]int{},
	}
}

// Checkout produces a worktree at commitish, fetching the mirror first.
//
// remote carries a credential and is used, never stored. identity is the
// unauthenticated remote: it names the mirror, so a repository whose remote
// changes gets a new one rather than a stale one.
//
// The returned release function removes the worktree. It must be called.
func (c *Cache) Checkout(ctx context.Context, identity, remote, commitish string) (gitx.Repo, func(), error) {
	key := mirrorKey(identity)

	unlock := c.lock(key)
	defer unlock()

	mirror, err := c.usableMirror(ctx, filepath.Join(c.workDir, "mirrors", key+".git"), remote)
	if err != nil {
		return nil, nil, err
	}

	dir, err := os.MkdirTemp(filepath.Join(c.workDir, "tasks"), "task-*")
	if err != nil {
		return nil, nil, err
	}

	// MkdirTemp created it and git wants to create it itself.
	target := filepath.Join(dir, "repo")

	repo, err := mirror.AddWorktree(ctx, target, commitish)
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, nil, err
	}

	c.hold(key, 1)
	c.touch(mirror.Dir())

	release := func() {
		func() {
			// Retaking the mirror lock: worktree removal writes to the
			// mirror's metadata, and a concurrent fetch would race it.
			unlock := c.lock(key)
			defer unlock()

			if err := mirror.RemoveWorktree(context.WithoutCancel(ctx), target); err != nil {
				c.log.Warn("could not remove worktree", "dir", target, "error", err)
			}
			if err := os.RemoveAll(dir); err != nil {
				c.log.Warn("could not remove task directory", "dir", dir, "error", err)
			}
		}()

		// Both the lock and the use count are dropped before sweeping: the
		// sweep takes the lock itself, and a mirror still counted in use is
		// one the sweep would skip — including this one, which has just become
		// the best candidate it has.
		c.hold(key, -1)

		// Sweeping here rather than on a timer keeps the disk bounded without
		// a background goroutine to own and shut down, and a task has just
		// finished so the cost lands where nobody is waiting.
		c.maybeSweep(context.WithoutCancel(ctx))
	}

	return repo, release, nil
}

// sweepEvery is the shortest interval between two sweeps.
const sweepEvery = time.Minute

// maybeSweep sweeps unless one ran recently.
func (c *Cache) maybeSweep(ctx context.Context) {
	if c.budget <= 0 {
		return
	}

	now := time.Now()

	c.mu.Lock()
	due := now.Sub(c.lastSweep) >= sweepEvery
	if due {
		c.lastSweep = now
	}
	c.mu.Unlock()

	if due {
		c.Sweep(ctx)
	}
}

// Sweep removes least-recently-used mirrors until they fit the budget.
//
// Eviction costs one slow task when an evicted repository comes back. A full
// disk fails every task at once, on every repository, and needs a human. The
// trade is not close.
func (c *Cache) Sweep(ctx context.Context) {
	if c.budget <= 0 {
		return
	}

	root := filepath.Join(c.workDir, "mirrors")

	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}

	type mirror struct {
		key      string
		path     string
		size     int64
		lastUsed time.Time
	}

	var (
		mirrors []mirror
		total   int64
	)

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}

		path := filepath.Join(root, e.Name())

		size, err := dirSize(path)
		if err != nil {
			continue
		}

		mirrors = append(mirrors, mirror{
			key:      strings.TrimSuffix(e.Name(), ".git"),
			path:     path,
			size:     size,
			lastUsed: lastUsed(path),
		})

		total += size
	}

	if total <= c.budget {
		return
	}

	// Least recently used first.
	sort.Slice(mirrors, func(i, j int) bool { return mirrors[i].lastUsed.Before(mirrors[j].lastUsed) })

	for _, m := range mirrors {
		if total <= c.budget {
			return
		}

		// A mirror with a live worktree is never evicted: removing it would
		// pull the ground from under a running task, and the disk it would
		// free is about to be freed anyway.
		//
		// Both checks are needed. The counter covers this process, including
		// the moment between creating a task directory and git registering the
		// worktree in it. Git's own metadata covers every other process: a
		// second worker pointed at the same work directory knows nothing about
		// this one's tasks, and would otherwise delete the objects it is
		// reading.
		if c.held(m.key) || hasLiveWorktree(m.path) {
			continue
		}

		unlock := c.lock(m.key)

		if err := os.RemoveAll(m.path); err != nil {
			c.log.WarnContext(ctx, "could not evict mirror", "mirror", m.path, "error", err)
			unlock()

			continue
		}

		unlock()

		c.log.InfoContext(ctx, "evicted mirror to stay within the disk budget",
			"mirror", m.path, "bytes", m.size, "last_used", m.lastUsed)

		total -= m.size
	}
}

// hold adjusts the count of worktrees cut from a mirror.
func (c *Cache) hold(key string, delta int) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.inUse[key] += delta
	if c.inUse[key] <= 0 {
		delete(c.inUse, key)
	}
}

// held reports whether a mirror has worktrees in use.
func (c *Cache) held(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.inUse[key] > 0
}

// touch records that a mirror was used, which is what eviction orders by.
//
// A marker file rather than the directory's own mtime: git rewrites parts of a
// repository during a fetch and leaves others alone for months, so directory
// timestamps say when git last happened to write, not when nit last needed it.
func (c *Cache) touch(dir string) {
	path := filepath.Join(dir, lastUsedFile)

	if err := os.WriteFile(path, nil, 0o600); err != nil {
		c.log.Warn("could not record mirror use", "mirror", dir, "error", err)
		return
	}
}

const lastUsedFile = "nit-last-used"

// hasLiveWorktree reports whether any worktree cut from this mirror still
// exists on disk.
//
// It reads what git itself reads: worktrees/<name>/gitdir holds the path of the
// worktree's .git file. An entry whose path is gone is stale and holds nothing.
//
// A task killed hard leaves both the entry and the directory, which pins its
// mirror until an operator clears the work directory. That is the safe
// direction to be wrong in — the alternative is deleting a repository out from
// under a task that is merely slow.
func hasLiveWorktree(mirrorDir string) bool {
	root := filepath.Join(mirrorDir, "worktrees")

	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}

	for _, e := range entries {
		gitdir, err := os.ReadFile(filepath.Join(root, e.Name(), "gitdir"))
		if err != nil {
			continue
		}

		if _, err := os.Stat(strings.TrimSpace(string(gitdir))); err == nil {
			return true
		}
	}

	return false
}

func lastUsed(dir string) time.Time {
	info, err := os.Stat(filepath.Join(dir, lastUsedFile))
	if err != nil {
		// Never touched by this process: treat it as oldest, so a mirror left
		// by a previous version is the first to go rather than the last.
		return time.Time{}
	}

	return info.ModTime()
}

func dirSize(dir string) (int64, error) {
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
			// A file removed under the walk is not an error worth failing on;
			// the next sweep will see the result.
			return nil
		}

		total += info.Size()

		return nil
	})

	return total, err
}

// Prepare makes sure the directory layout exists, so a task does not fail on a
// missing parent at the least convenient moment.
func (c *Cache) Prepare() error {
	for _, dir := range []string{"mirrors", "tasks"} {
		if err := os.MkdirAll(filepath.Join(c.workDir, dir), 0o750); err != nil {
			return err
		}
	}

	return nil
}

// usableMirror returns a mirror that has just been fetched, rebuilding it once
// if anything about the existing one is unusable.
//
// Both steps can fail on a corrupt mirror and the first one is easy to
// overlook: an interrupted pack write leaves objects/ in a state where `git
// init --bare` itself refuses, so a rebuild that only triggered on a failed
// fetch would never run. That was found by a test that corrupted a mirror on
// purpose, which is the only way this path is ever exercised before a customer
// exercises it.
//
// Rebuilding costs one clone. Not rebuilding poisons every later task for this
// repository — the kind of failure that takes a day to understand, because
// nothing about "push is failing" points at a directory on a worker.
func (c *Cache) usableMirror(ctx context.Context, dir, remote string) (gitx.Mirror, error) {
	mirror, err := c.git.Mirror(ctx, dir)
	if err == nil {
		if err = mirror.Fetch(ctx, remote); err == nil {
			return mirror, nil
		}
	}

	c.log.WarnContext(ctx, "mirror unusable, rebuilding", "mirror", dir, "error", err)

	if err := os.RemoveAll(dir); err != nil {
		return nil, err
	}

	mirror, err = c.git.Mirror(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("mirror could not be rebuilt: %w", err)
	}

	if err := mirror.Fetch(ctx, remote); err != nil {
		return nil, fmt.Errorf("mirror could not be rebuilt: %w", err)
	}

	return mirror, nil
}

// lock takes the per-mirror lock and returns its release.
func (c *Cache) lock(key string) func() {
	c.mu.Lock()

	m, ok := c.mirrors[key]
	if !ok {
		m = &sync.Mutex{}
		c.mirrors[key] = m
	}

	c.mu.Unlock()

	m.Lock()

	return m.Unlock
}

// mirrorKey names a mirror's directory.
//
// A hash of the remote rather than the remote itself: a URL is not a path, and
// more importantly an authenticated one carries a token — a directory listing
// is not where a credential should be readable. Hashing the *unauthenticated*
// remote also means a repository whose remote moves gets a fresh mirror
// instead of one pointed at the wrong place.
func mirrorKey(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:8])
}
