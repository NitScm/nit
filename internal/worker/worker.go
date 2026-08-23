// Package worker executes the tasks the control plane queues.
//
// Both kinds live here because they share almost everything: the clone cache,
// the patch pipeline, the sync point bookkeeping. Splitting them into two
// binaries would double the deployment surface to share ninety percent of the
// code; scaling is per queue instead (`nit-worker --queues=pull`).
//
// The two differ in one important way. A push worker re-derives no
// authorization: the control plane already decided, refused what had to be
// refused, and stored the *enforced* patch. A pull worker does the filtering
// itself, because what is readable has to be decided against the bundle in
// force when the diff is produced, not when the request was accepted.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/NitScm/nit/internal/auditlog"
	"github.com/NitScm/nit/internal/blob"
	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/internal/store"
	"github.com/NitScm/nit/internal/synctoken"
	"github.com/NitScm/nit/pkg/audit"
	"github.com/NitScm/nit/pkg/enforce"
	"github.com/NitScm/nit/pkg/forge"
	"github.com/NitScm/nit/pkg/gitx"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
)

// Config holds a worker's tunables.
type Config struct {
	// WorkDir is where clones live.
	WorkDir string

	Tenant policy.TenantID

	// PullArtifactTTL is how long a generated patch stays fetchable. It must
	// comfortably exceed the time a developer might take to notice their pull
	// finished.
	PullArtifactTTL time.Duration

	// MaxPatchBytes caps a produced patch.
	MaxPatchBytes int64

	// Guards are the structural protections. The push path does not use them —
	// the control plane already applied them — but a pull filters with the same
	// configuration so the two stay describable by one policy.
	Guards enforce.Guards

	// Credentials authenticate against the forge. nit is the only writer of the
	// upstream, so this is a machine identity, not a user's.
	Credentials forge.Credentials
}

func (c *Config) applyDefaults() {
	if c.WorkDir == "" {
		c.WorkDir = filepath.Join(os.TempDir(), "nit-worker")
	}
	if c.Tenant == "" {
		c.Tenant = policy.DefaultTenant
	}
	if c.PullArtifactTTL <= 0 {
		c.PullArtifactTTL = 24 * time.Hour
	}
	if c.MaxPatchBytes <= 0 {
		c.MaxPatchBytes = 100 << 20
	}
	if len(c.Guards.ProtectedPaths) == 0 && !c.Guards.SymlinksRequireAdmin && !c.Guards.SubmodulesRequireAdmin {
		c.Guards = enforce.DefaultGuards()
	}
}

// Deps are a worker's collaborators.
type Deps struct {
	Store      store.Store
	Blobs      blob.Store
	Git        gitx.Git
	Forges     *forge.Registry
	Policy     policyloader.Source
	SyncTokens *synctoken.Signer
	Log        *slog.Logger
	Now        func() time.Time

	// AuditSink receives every decision in addition to the database. Nil means
	// persist only, which is the default and what most deployments run.
	AuditSink audit.Sink
}

// Worker executes tasks.
type Worker struct {
	cfg  Config
	deps Deps

	// audit persists every decision and forwards it, and cannot fail a task
	// while doing so.
	audit *auditlog.Recorder
}

// New wires a worker.
func New(cfg Config, deps Deps) (*Worker, error) {
	cfg.applyDefaults()

	switch {
	case deps.Store == nil:
		return nil, errors.New("worker: no store")
	case deps.Blobs == nil:
		return nil, errors.New("worker: no blob store")
	case deps.Git == nil:
		return nil, errors.New("worker: no git")
	case deps.Policy == nil:
		return nil, errors.New("worker: no policy source")
	case deps.SyncTokens == nil:
		return nil, errors.New("worker: no sync token signer")
	}

	if deps.Forges == nil {
		deps.Forges = forge.NewRegistry()
	}
	if deps.Log == nil {
		deps.Log = slog.Default()
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now().UTC() }
	}

	if err := os.MkdirAll(cfg.WorkDir, 0o750); err != nil {
		return nil, fmt.Errorf("worker: create work dir: %w", err)
	}

	return &Worker{
		cfg:   cfg,
		deps:  deps,
		audit: auditlog.New(deps.Store.Audit(), deps.AuditSink, deps.Log),
	}, nil
}

// Handle executes a task and returns its marshalled result. It is the
// queue.Handler of this package.
func (w *Worker) Handle(ctx context.Context, task *store.Task) ([]byte, error) {
	switch task.Kind {
	case protocol.TaskPush:
		return w.handlePush(ctx, task)
	case protocol.TaskPull:
		return w.handlePull(ctx, task)
	default:
		return nil, permanent(protocol.CodeUnsupportedVersion, "unknown task kind %q", task.Kind)
	}
}

// clone prepares a working clone of a repository.
//
// Every task gets a fresh clone, and it is removed afterwards. A cache keyed by
// repository would be a large win on big repositories, and it is the obvious
// next optimization — but a cache shared between tasks that apply patches and
// rebase is also a way for one task's leftover state to corrupt another's, and
// that trade is not worth taking before the correctness is settled.
func (w *Worker) clone(ctx context.Context, remote, branch string) (gitx.Repo, func(), error) {
	dir, err := os.MkdirTemp(w.cfg.WorkDir, "task-*")
	if err != nil {
		return nil, nil, err
	}

	cleanup := func() {
		if err := os.RemoveAll(dir); err != nil {
			w.deps.Log.Warn("could not remove work directory", "dir", dir, "error", err)
		}
	}

	// The clone directory must not exist for git; MkdirTemp created it.
	target := filepath.Join(dir, "repo")

	repo, err := w.deps.Git.Clone(ctx, remote, target, gitx.CloneOptions{Branch: branch})
	if err != nil {
		cleanup()

		// The remote may carry a token. Never surface the underlying message.
		return nil, nil, fmt.Errorf("clone failed for branch %q", branch)
	}

	return repo, cleanup, nil
}

// authenticatedRemote resolves the URL git should use.
func (w *Worker) authenticatedRemote(ctx context.Context, forgeKind, remote string) (string, error) {
	driver := w.deps.Forges.Get(forgeKind)

	authenticated, err := driver.AuthenticatedRemote(ctx, forge.RepoRef{Remote: remote}, w.cfg.Credentials)
	if err != nil {
		return "", fmt.Errorf("resolving the remote failed")
	}

	return authenticated, nil
}

// loadSpec decodes a task payload.
func loadSpec[T any](task *store.Task) (T, error) {
	var spec T

	if err := json.Unmarshal(task.Payload, &spec); err != nil {
		return spec, permanent("malformed_task", "the task payload could not be decoded: %v", err)
	}

	return spec, nil
}

// permanent builds an error the queue will not retry.
//
// The distinction matters: a conflict or a denial will fail identically on
// every attempt, and retrying it wastes a clone while the developer waits for
// an answer that is not going to change.
func permanent(code, format string, args ...any) error {
	return &protocol.Error{Code: code, Message: fmt.Sprintf(format, args...)}
}
