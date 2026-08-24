// Package nitd assembles and runs nit's server-side processes.
//
// It is what `cmd/nitd` and `cmd/nit-worker` are built on, and what anyone
// embedding nit in their own binary uses. Both shipped binaries call it with a
// zero Deps, which is deliberate: if the façade were a second path used only by
// outsiders, it would drift from what nit actually does within a release.
//
// # Why this package exists
//
// Everything that assembles a server — the queue, the auth service, the policy
// loader, the store connection — lives under internal/, so nothing outside this
// module could put one together. That is correct for the parts and wrong for
// the whole: a store backend, a blob store or a policy source can all be
// written out of tree, and until this package existed there was no way to
// actually run one.
//
// # The exception it takes to the pkg/ rule
//
// `pkg/` performs no IO. This package does: it opens a database, reads a
// directory, listens on a socket. The rule exists so the authorization path
// stays testable without infrastructure, and the way this package keeps that
// promise is by being a leaf — nothing else in the module imports it, and a
// test in this package asserts it. See docs/DECISIONS.md D38.
//
// # Supplying your own parts
//
// Every Deps field may be nil, and a nil field is built from Config exactly as
// the shipped binaries build it. So:
//
//	nitd.Serve(ctx, cfg, nitd.Deps{})              // what `nitd` does
//	nitd.Serve(ctx, cfg, nitd.Deps{Blobs: mine})   // the same, on your storage
//
// What you supply, you own: a Store or a Blobs you pass in is not closed here,
// because a caller that shares one between a server and a worker in one process
// would have it closed underneath them. What this package opens, it closes.
package nitd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/NitScm/nit/internal/auditbuf"
	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/internal/blob/filesystem"
	"github.com/NitScm/nit/internal/bootstrap"
	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/internal/queue"
	"github.com/NitScm/nit/internal/server"
	"github.com/NitScm/nit/internal/store/connect"
	"github.com/NitScm/nit/internal/synctoken"
	"github.com/NitScm/nit/internal/worker"
	"github.com/NitScm/nit/pkg/audit"
	"github.com/NitScm/nit/pkg/blob"
	"github.com/NitScm/nit/pkg/forge"
	"github.com/NitScm/nit/pkg/gitx"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
	"github.com/NitScm/nit/pkg/pullcache"
	"github.com/NitScm/nit/pkg/store"
)

// Config is the effective configuration of a nit process.
//
// An alias rather than a copy. The configuration was already a public contract
// — it is one-to-one with nit.yaml and every field is documented in
// docs/CONFIGURATION.md — so the alias makes that visible instead of creating
// it, and a second struct would drift from the one the file maps to.
type Config = bootstrap.Config

// Load reads the same configuration the binaries read: the file, then the
// environment, over the built-in defaults.
//
// An empty path searches $NIT_CONFIG, ./nit.yaml, ~/.config/nit/nit.yaml and
// /etc/nit/nit.yaml, in that order.
func Load(path string) (Config, error) { return bootstrap.LoadConfigFrom(path) }

// NewLogger builds the logger the binaries use, at the configured level.
func NewLogger(level slog.Level) *slog.Logger { return bootstrap.NewLogger(level) }

// Deps are the parts a caller may supply instead of the ones Config describes.
//
// Every field is optional. A nil field is built from Config exactly as the
// shipped binaries build it, so a zero Deps is not a degraded mode — it is the
// mode.
type Deps struct {
	// Store is the control plane's state. Nil opens Config.DatabaseURL.
	Store store.Store

	// Blobs holds patch payloads. Nil uses a directory at Config.BlobDir.
	//
	// A server and its workers must share one. Two processes with two local
	// directories produce missing_patch on every push, which is the single
	// most common way to get a deployment wrong — and the reason an
	// object-storage implementation is worth writing.
	Blobs blob.Store

	// Policy is the bundle in force. Nil watches Config.PolicyDir.
	//
	// Supplying one takes over its refresh: this package will not start a
	// watcher for a source it did not create, because a source that fetches
	// from elsewhere knows better than a timer when it has changed.
	Policy policy.Source

	// AuditSink receives every decision in addition to the database. Nil
	// persists only, which is the default and what most deployments run.
	AuditSink audit.Sink

	Log *slog.Logger

	// Now is the clock. Nil is time.Now in UTC.
	Now func() time.Time
}

// WorkerDeps adds what only a worker needs.
type WorkerDeps struct {
	Deps

	// Git runs the git operations. Nil uses the git on PATH, with
	// Config.GitSSHCommand appended to its environment.
	Git gitx.Git

	// Forges resolve a repository's hosting provider. Nil uses the built-in
	// registry.
	Forges *forge.Registry

	// PullCache shares a filtered projection between users whose read rights
	// are identical. Nil uses the per-process cache that ships with nit, which
	// already collapses a release-day herd on a fleet small enough that the
	// herd lands on the same few workers.
	//
	// What a replacement must not reimplement is the key: policy.Profile
	// decides who may share a projection, and its correctness is authorization
	// correctness. See pkg/pullcache.
	PullCache pullcache.Store
}

// WorkerOptions are the per-process choices that have no place in a
// configuration file, because they differ between two workers reading the same
// one.
type WorkerOptions struct {
	// Name is recorded on leases, so an operator reading `nitctl tasks` can
	// tell which machine holds what. Empty uses the hostname.
	Name string

	// Concurrency is the number of runners in this process. Each holds one
	// worktree at a time, which is what keeps a worker's disk predictable.
	// Zero means one.
	Concurrency int

	// Kinds restricts what this process takes. Empty means both, which is also
	// what lets the store skip the filter.
	Kinds []protocol.TaskKind
}

// Serve runs the control plane until ctx is done.
//
// It does not trap signals. A caller that wants Ctrl-C to stop it wraps ctx in
// signal.NotifyContext, which is what cmd/nitd does — a process embedding nit
// alongside other things has its own idea of what a signal means.
func Serve(ctx context.Context, cfg Config, deps Deps) error {
	parts, err := open(ctx, cfg, deps)
	if err != nil {
		return err
	}
	defer parts.close()

	signer, err := tenantSigner(cfg)
	if err != nil {
		return err
	}

	q := queue.New(parts.store.Tasks(), queue.Options{
		LeaseFor:    cfg.LeaseDuration,
		MaxAttempts: cfg.MaxAttempts,
		PollEvery:   cfg.QueuePoll,
	})

	srv, err := server.New(server.Config{
		Addr:            cfg.Addr,
		Tenant:          policy.DefaultTenant,
		MaxPatchBytes:   cfg.MaxPatchBytes,
		AdminGroups:     cfg.AdminGroups,
		AllowedOrigins:  cfg.CORSOrigins,
		PullArtifactTTL: cfg.PullTTL,
		EventMaxWait:    cfg.EventMaxWait,
	}, server.Deps{
		Store:      parts.store,
		Queue:      q,
		Blobs:      parts.blobs,
		Policy:     policy.OneSource{Source: parts.policy},
		Auth:       auth.NewService(parts.store, policy.OneSource{Source: parts.policy}, policy.DefaultTenant, nil),
		SyncTokens: signer,
		Log:        parts.log,
		AuditSink:  parts.auditSink,
	})
	if err != nil {
		return err
	}

	var wg sync.WaitGroup

	// The reaper returns tasks abandoned by dead workers to the queue. Without
	// it a crashed worker's branch stays blocked until something else happens
	// to claim.
	background(&wg, func() {
		if err := queue.NewReaper(q, cfg.ReapEvery, parts.log).Run(ctx); err != nil {
			parts.log.Error("reaper stopped", "error", err)
		}
	})

	parts.watchPolicy(ctx, &wg, cfg)

	parts.log.Info("nitd starting",
		"addr", cfg.Addr,
		"config", configOrigin(cfg.ConfigFile),
		"admin_groups", cfg.AdminGroups,
		"policy", parts.policy.Current().Version(),
		"repositories", len(parts.policy.Current().Repositories()))

	err = srv.ListenAndServe(ctx)

	wg.Wait()

	parts.log.Info("nitd stopped")

	return err
}

// Work runs a worker until ctx is done.
func Work(ctx context.Context, cfg Config, opts WorkerOptions, deps WorkerDeps) error {
	parts, err := open(ctx, cfg, deps.Deps)
	if err != nil {
		return err
	}
	defer parts.close()

	signer, err := tenantSigner(cfg)
	if err != nil {
		return err
	}

	git := deps.Git
	version := "supplied"

	if git == nil {
		exec := gitx.NewExecGit()

		// Appended after the inherited environment, so a configured value
		// overrides a GIT_SSH_COMMAND the host happens to export. A setting
		// that silently did nothing on such a host would be worse than no
		// setting at all.
		if cfg.GitSSHCommand != "" {
			exec.Env = append(exec.Env, "GIT_SSH_COMMAND="+cfg.GitSSHCommand)
		}

		if version, err = exec.Version(ctx); err != nil {
			return fmt.Errorf("git is not usable: %w", err)
		}

		git = exec
	}

	forges := deps.Forges
	if forges == nil {
		forges = forge.NewRegistry()
	}

	w, err := worker.New(worker.Config{
		WorkDir:           cfg.WorkDir,
		MirrorBudgetBytes: cfg.MirrorBudgetBytes,
		Tenant:            policy.DefaultTenant,
		MaxPatchBytes:     cfg.MaxPatchBytes,
		PullArtifactTTL:   cfg.PullTTL,
		Credentials:       forge.Credentials{Token: cfg.ForgeToken},
	}, worker.Deps{
		Store:      parts.store,
		Blobs:      parts.blobs,
		Git:        git,
		Forges:     forges,
		Policy:     policy.OneSource{Source: parts.policy},
		SyncTokens: signer,
		Log:        parts.log,
		AuditSink:  parts.auditSink,
		Now:        deps.Now,
		PullCache:  deps.PullCache,
	})
	if err != nil {
		return err
	}

	q := queue.New(parts.store.Tasks(), queue.Options{
		LeaseFor:    cfg.LeaseDuration,
		MaxAttempts: cfg.MaxAttempts,
		PollEvery:   cfg.QueuePoll,
	})

	name := opts.Name
	if name == "" {
		name = hostname()
	}

	concurrency := max(1, opts.Concurrency)

	parts.log.Info("nit-worker starting",
		"name", name,
		"config", configOrigin(cfg.ConfigFile),
		"lease", cfg.LeaseDuration,
		"concurrency", concurrency,
		"git", version,
		"policy", parts.policy.Current().Version())

	var wg sync.WaitGroup

	// Concurrency comes from running several runners, not from fanning out
	// inside one: each holds one worktree at a time, which keeps the disk
	// budget of a worker something an operator can reason about.
	for i := range concurrency {
		holder := fmt.Sprintf("%s-%d", name, i)

		background(&wg, func() {
			if err := queue.NewRunner(q, holder, w.Handle, parts.log, opts.Kinds...).Run(ctx); err != nil {
				parts.log.Error("runner stopped", "holder", holder, "error", err)
			}
		})
	}

	parts.watchPolicy(ctx, &wg, cfg)

	wg.Wait()

	parts.log.Info("nit-worker stopped")

	return nil
}

// parts is what a process needs, whether supplied or built here.
type parts struct {
	store  store.Store
	blobs  blob.Store
	policy policy.Source
	log    *slog.Logger

	// auditSink is what a caller supplied, with a queue in front of it. Nil
	// when nothing was supplied, which stays the ordinary case.
	auditSink audit.Sink

	// loader is set only when this package created the policy source, and is
	// what decides whether a watcher runs.
	loader *policyloader.Loader

	// closers are the things opened here. What a caller supplied is theirs.
	closers []func()
}

func open(ctx context.Context, cfg Config, deps Deps) (*parts, error) {
	p := &parts{
		store:  deps.Store,
		blobs:  deps.Blobs,
		policy: deps.Policy,
		log:    deps.Log,
	}

	if p.log == nil {
		p.log = bootstrap.NewLogger(cfg.LogLevel)
	}

	// A sink is exported to, not consulted, so it has no business on the
	// request path. Buffering here rather than asking every implementation to
	// do it is what lets a sink be a client that speaks one protocol: pkg/audit
	// used to make "buffers and returns" the implementer's obligation, and an
	// obligation met once per destination is met differently once per
	// destination. New returns nil for a nil sink, so no export configured
	// stays the absence of a sink.
	if buffered := auditbuf.New(deps.AuditSink, auditbuf.Options{Log: p.log}); buffered != nil {
		p.auditSink = buffered
		p.closers = append(p.closers, func() { buffered.Close() })
	}

	if p.store == nil {
		if cfg.DatabaseURL == "" {
			return nil, errors.New(bootstrap.EnvDatabaseURL + " is required")
		}

		opened, err := connect.Open(ctx, cfg.DatabaseURL)
		if err != nil {
			return nil, err
		}

		p.store = opened
		p.closers = append(p.closers, func() { opened.Close() })
	}

	if p.policy == nil {
		loader, err := policyloader.New(cfg.PolicyDir, p.log)
		if err != nil {
			p.close()
			return nil, err
		}

		p.loader = loader
		p.policy = loader
	}

	if p.blobs == nil {
		blobs, err := tenantBlobs(cfg.BlobDir, policy.DefaultTenant)
		if err != nil {
			p.close()
			return nil, err
		}

		p.blobs = blobs
	}

	// Reconciled whichever source the bundle came from: a task, an audit record
	// and a sync point all reference a repository row, and a bundle entry with
	// no row is rejected by the API as an unknown repository.
	if err := bootstrap.ReconcilePolicy(ctx, p.store, p.policy.Current(), policy.DefaultTenant); err != nil {
		p.close()
		return nil, err
	}

	if p.loader != nil {
		// A repository added to the bundle gets its row without anyone running
		// a command; a hot reload would otherwise leave the API rejecting it.
		p.loader.OnReload = func(reloaded *policy.Policy) {
			if err := bootstrap.ReconcilePolicy(
				context.WithoutCancel(ctx), p.store, reloaded, policy.DefaultTenant); err != nil {
				p.log.Error("reconcile after policy reload failed", "error", err)
			}
		}
	}

	return p, nil
}

// watchPolicy starts the reload loop, but only for a source this package
// created. A caller who supplied one is responsible for refreshing it, and a
// timer here would either duplicate their work or do nothing.
func (p *parts) watchPolicy(ctx context.Context, wg *sync.WaitGroup, cfg Config) {
	if p.loader == nil {
		return
	}

	background(wg, func() {
		if err := p.loader.Watch(ctx, cfg.PolicyReload); err != nil {
			p.log.Error("policy watcher stopped", "error", err)
		}
	})
}

func (p *parts) close() {
	for i := len(p.closers) - 1; i >= 0; i-- {
		p.closers[i]()
	}

	p.closers = nil
}

// configOrigin describes where settings came from, for the start-up line.
func configOrigin(path string) string {
	if path == "" {
		return "environment and defaults"
	}
	return path
}

func background(wg *sync.WaitGroup, fn func()) {
	wg.Add(1)

	go func() {
		defer wg.Done()
		fn()
	}()
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "worker"
	}

	return name
}

// tenantSigner derives the signer for the tenant this process serves.
//
// One tenant today, and the derivation is what makes that a *statement* rather
// than an assumption: a token minted here carries a key that only this tenant's
// signer reproduces, so the day a second tenant exists, misrouting one fails
// closed instead of succeeding quietly.
func tenantSigner(cfg Config) (*synctoken.Signer, error) {
	root, err := synctoken.NewRoot(cfg.SyncKey)
	if err != nil {
		return nil, err
	}

	return root.For(policy.DefaultTenant)
}

// tenantBlobs roots the blob store under the tenant that owns it.
//
// The namespace used to be flat, so two tenants pushing identical bytes shared
// a blob. Today that is unreachable — a patch is fetched through its task and
// never through a bare digest — but it is a deduplication side channel waiting
// for the first endpoint that takes a digest, and cross-tenant dedup is worth
// nothing.
//
// The fallback is what makes the move invisible to a running deployment. Blobs
// written before it sit one directory up; reads find them there until the last
// one expires, and the fallback can then be removed with the old directory.
func tenantBlobs(root string, tenant policy.TenantID) (blob.Store, error) {
	scoped, err := filesystem.New(filepath.Join(root, string(tenant)))
	if err != nil {
		return nil, err
	}

	legacy, err := filesystem.New(root)
	if err != nil {
		return nil, err
	}

	return blob.Fallback{Primary: scoped, Secondary: legacy}, nil
}
