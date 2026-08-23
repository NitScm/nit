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
	"sync"

	"github.com/NitScm/nit/pkg/gitx"
)

// Cache hands out worktrees backed by shared mirrors.
type Cache struct {
	git     gitx.Git
	workDir string
	log     *slog.Logger

	// mu guards mirrors; each mirror has its own lock because git serializes
	// ref updates per repository, not globally: two tasks fetching into one
	// mirror race on ref locks, two tasks on different mirrors do not.
	mu      sync.Mutex
	mirrors map[string]*sync.Mutex
}

// New returns a cache rooted at workDir.
func New(git gitx.Git, workDir string, log *slog.Logger) *Cache {
	if log == nil {
		log = slog.Default()
	}

	return &Cache{
		git:     git,
		workDir: workDir,
		log:     log,
		mirrors: map[string]*sync.Mutex{},
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

	release := func() {
		// Retaking the mirror lock: worktree removal writes to the mirror's
		// metadata, and a concurrent fetch would race it.
		unlock := c.lock(key)
		defer unlock()

		if err := mirror.RemoveWorktree(context.WithoutCancel(ctx), target); err != nil {
			c.log.Warn("could not remove worktree", "dir", target, "error", err)
		}
		if err := os.RemoveAll(dir); err != nil {
			c.log.Warn("could not remove task directory", "dir", dir, "error", err)
		}
	}

	return repo, release, nil
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
