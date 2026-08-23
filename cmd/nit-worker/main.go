// Command nit-worker executes queued tasks: it clones, applies, filters and
// pushes.
//
// One binary handles both kinds. Push and pull share the clone handling, the
// patch pipeline and the sync point bookkeeping, so splitting them would double
// the deployment surface for ninety percent shared code. Scaling is per queue:
//
//	nit-worker                    take both kinds
//	nit-worker -queues=pull       dedicate a machine to read traffic
//	nit-worker -concurrency=4     run four runners in this process
//
// Configuration comes from the same environment as nitd, so the two cannot
// disagree about the database, the policy bundle or the sync token key:
//
//	NIT_DATABASE_URL, NIT_POLICY_DIR, NIT_BLOB_DIR, NIT_SYNC_KEY, NIT_WORK_DIR
//	NIT_FORGE_TOKEN    credential nit uses to push to the forge
//	NIT_LEASE_DURATION how long a task may run without a heartbeat
//
// docs/CONFIGURATION.md is the reference.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	"github.com/NitScm/nit/internal/blob/filesystem"
	"github.com/NitScm/nit/internal/bootstrap"
	"github.com/NitScm/nit/internal/buildinfo"
	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/internal/queue"
	"github.com/NitScm/nit/internal/store/connect"
	"github.com/NitScm/nit/internal/synctoken"
	"github.com/NitScm/nit/internal/worker"
	"github.com/NitScm/nit/pkg/forge"
	"github.com/NitScm/nit/pkg/gitx"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/protocol"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "nit-worker:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	fs := flag.NewFlagSet("nit-worker", flag.ContinueOnError)

	showVersion := fs.Bool("version", false, "print the build identity and exit")

	queues := fs.String("queues", "push,pull", "task kinds this worker takes")
	concurrency := fs.Int("concurrency", 1, "runners in this process")
	name := fs.String("name", hostname(), "identifier recorded on leases")
	configFile := fs.String("config", "", "configuration file (see docs/CONFIGURATION.md)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Println("nit-worker", buildinfo.Get())
		return nil
	}

	cfg, err := bootstrap.LoadConfigFrom(*configFile)
	if err != nil {
		return err
	}
	if cfg.DatabaseURL == "" {
		return errors.New(bootstrap.EnvDatabaseURL + " is required")
	}

	log := bootstrap.NewLogger(cfg.LogLevel)

	kinds, err := parseKinds(*queues)
	if err != nil {
		return err
	}

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

	blobs, err := filesystem.New(cfg.BlobDir)
	if err != nil {
		return err
	}

	signer, err := synctoken.NewSigner(cfg.SyncKey)
	if err != nil {
		return err
	}

	git := gitx.NewExecGit()

	// Appended after the inherited environment, so a configured value overrides
	// a GIT_SSH_COMMAND the host happens to export. A setting that silently did
	// nothing on such a host would be worse than no setting at all. Left unset,
	// nothing is appended and the inherited configuration stands.
	if cfg.GitSSHCommand != "" {
		git.Env = append(git.Env, "GIT_SSH_COMMAND="+cfg.GitSSHCommand)
	}

	version, err := git.Version(ctx)
	if err != nil {
		return fmt.Errorf("git is not usable: %w", err)
	}

	w, err := worker.New(worker.Config{
		WorkDir:           cfg.WorkDir,
		MirrorBudgetBytes: cfg.MirrorBudgetBytes,
		Tenant:            policy.DefaultTenant,
		MaxPatchBytes:     cfg.MaxPatchBytes,
		PullArtifactTTL:   cfg.PullTTL,
		Credentials:       forge.Credentials{Token: cfg.ForgeToken},
	}, worker.Deps{
		Store:      st,
		Blobs:      blobs,
		Git:        git,
		Forges:     forge.NewRegistry(),
		Policy:     loader,
		SyncTokens: signer,
		Log:        log,
	})
	if err != nil {
		return err
	}

	q := queue.New(st.Tasks(), queue.Options{
		LeaseFor:    cfg.LeaseDuration,
		MaxAttempts: cfg.MaxAttempts,
		PollEvery:   cfg.QueuePoll,
	})

	log.Info("nit-worker starting",
		"name", *name,
		"config", cfg.ConfigFile,
		"lease", cfg.LeaseDuration,
		"queues", *queues,
		"concurrency", *concurrency,
		"git", version,
		"policy", loader.Current().Version())

	var wg sync.WaitGroup

	// Concurrency comes from running several runners, not from fanning out
	// inside one: each runner holds one clone at a time, which keeps the disk
	// budget of a worker something an operator can reason about.
	for i := range max(1, *concurrency) {
		holder := fmt.Sprintf("%s-%d", *name, i)

		wg.Add(1)

		go func() {
			defer wg.Done()

			if err := queue.NewRunner(q, holder, w.Handle, log, kinds...).Run(ctx); err != nil {
				log.Error("runner stopped", "holder", holder, "error", err)
			}
		}()
	}

	wg.Add(1)

	go func() {
		defer wg.Done()

		if err := loader.Watch(ctx, cfg.PolicyReload); err != nil {
			log.Error("policy watcher stopped", "error", err)
		}
	}()

	wg.Wait()

	log.Info("nit-worker stopped")

	return nil
}

func parseKinds(spec string) ([]protocol.TaskKind, error) {
	var kinds []protocol.TaskKind

	for part := range strings.SplitSeq(spec, ",") {
		switch kind := protocol.TaskKind(strings.TrimSpace(part)); kind {
		case protocol.TaskPush, protocol.TaskPull:
			kinds = append(kinds, kind)
		case "":
		default:
			return nil, fmt.Errorf("unknown queue %q", part)
		}
	}

	if len(kinds) == 0 {
		return nil, errors.New("-queues names no known queue")
	}

	// Both kinds means no restriction, which lets the store skip the filter.
	if len(kinds) == 2 {
		return nil, nil
	}

	return kinds, nil
}

func hostname() string {
	name, err := os.Hostname()
	if err != nil {
		return "worker"
	}
	return name
}
