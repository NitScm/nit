// Command nitd is the nit control plane.
//
// It owns identity, authorization, sync points, the task queue, the blob store
// and the audit log. It performs no git operation: anything that clones,
// applies or pushes runs in a worker, so a slow or hostile repository can never
// block the API.
//
// Configuration comes from a file and the environment, in that order of
// precedence, with built-in defaults underneath. See docs/CONFIGURATION.md.
//
//	nitd -config /etc/nit/nit.yaml
//
// The environment variables, all optional except the two marked:
//
//	NIT_ADDR           listen address                      (default :8080)
//	NIT_DATABASE_URL   PostgreSQL DSN                       (required)
//	NIT_POLICY_DIR     policy bundle directory              (required in practice)
//	NIT_BLOB_DIR       where patch payloads are stored      (default ./var/blobs)
//	NIT_SYNC_KEY       sync token signing key, >= 32 bytes  (required)
//	NIT_ADMIN_GROUPS   groups allowed to read the operations API
//	NIT_CORS_ORIGINS   browser origins allowed to call the API
//	NIT_MAX_PATCH_BYTES, NIT_LOG_LEVEL
//	NIT_LEASE_DURATION, NIT_MAX_ATTEMPTS, NIT_QUEUE_POLL, NIT_REAP_EVERY,
//	NIT_PULL_TTL, NIT_EVENT_MAX_WAIT, NIT_POLICY_RELOAD
//
// docs/CONFIGURATION.md is the reference.
//
// Migrations are not applied here. A schema change is a deployment step an
// operator decides to take: run "nitctl migrate" first.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/internal/blob/filesystem"
	"github.com/NitScm/nit/internal/bootstrap"
	"github.com/NitScm/nit/internal/buildinfo"
	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/internal/queue"
	"github.com/NitScm/nit/internal/server"
	"github.com/NitScm/nit/internal/store/connect"
	"github.com/NitScm/nit/internal/synctoken"
	"github.com/NitScm/nit/pkg/policy"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "nitd:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("nitd", flag.ContinueOnError)

	showVersion := fs.Bool("version", false, "print the build identity and exit")
	configFile := fs.String("config", "",
		"configuration file (default: $NIT_CONFIG, ./nit.yaml, ~/.config/nit/nit.yaml, /etc/nit/nit.yaml)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Println("nitd", buildinfo.Get())
		return nil
	}

	cfg, err := bootstrap.LoadConfigFrom(*configFile)
	if err != nil {
		return err
	}

	log := bootstrap.NewLogger(cfg.LogLevel)

	if cfg.DatabaseURL == "" {
		return errors.New(bootstrap.EnvDatabaseURL + " is required")
	}

	// Signals are trapped before anything long-running starts, so a Ctrl-C
	// during start-up is a clean exit rather than a half-initialized process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := connect.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()

	loader, err := policyloader.New(cfg.PolicyDir, log)
	if err != nil {
		return err
	}

	if err := bootstrap.ReconcilePolicy(ctx, st, loader.Current(), policy.DefaultTenant); err != nil {
		return err
	}

	// A repository added to the bundle gets its row without anyone running a
	// command; a hot reload would otherwise leave the API rejecting it as
	// unknown.
	loader.OnReload = func(p *policy.Policy) {
		if err := bootstrap.ReconcilePolicy(context.WithoutCancel(ctx), st, p, policy.DefaultTenant); err != nil {
			log.Error("reconcile after policy reload failed", "error", err)
		}
	}

	blobs, err := filesystem.New(cfg.BlobDir)
	if err != nil {
		return err
	}

	signer, err := synctoken.NewSigner(cfg.SyncKey)
	if err != nil {
		return err
	}

	q := queue.New(st.Tasks(), queue.Options{
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
		Store:      st,
		Queue:      q,
		Blobs:      blobs,
		Policy:     loader,
		Auth:       auth.NewService(st, loader, policy.DefaultTenant, nil),
		SyncTokens: signer,
		Log:        log,
	})
	if err != nil {
		return err
	}

	var wg sync.WaitGroup

	// The reaper returns tasks abandoned by dead workers to the queue. Without
	// it a crashed worker's branch stays blocked until something else happens
	// to claim.
	background(&wg, func() {
		if err := queue.NewReaper(q, cfg.ReapEvery, log).Run(ctx); err != nil {
			log.Error("reaper stopped", "error", err)
		}
	})

	background(&wg, func() {
		if err := loader.Watch(ctx, cfg.PolicyReload); err != nil {
			log.Error("policy watcher stopped", "error", err)
		}
	})

	log.Info("nitd starting",
		"addr", cfg.Addr,
		"config", configOrigin(cfg.ConfigFile),
		"admin_groups", cfg.AdminGroups,
		"policy", loader.Current().Version(),
		"repositories", len(loader.Current().Repositories()))

	serveErr := srv.ListenAndServe(ctx)

	wg.Wait()

	log.Info("nitd stopped")

	return serveErr
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
