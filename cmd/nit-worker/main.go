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
	"syscall"

	"github.com/NitScm/nit/internal/buildinfo"
	"github.com/NitScm/nit/pkg/nitd"
	"github.com/NitScm/nit/pkg/protocol"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "nit-worker:", err)
		os.Exit(1)
	}
}

// run is deliberately thin: the assembly lives in pkg/nitd, and this binary
// calls the same entry point an out-of-tree worker does.
func run(args []string) error {
	fs := flag.NewFlagSet("nit-worker", flag.ContinueOnError)

	showVersion := fs.Bool("version", false, "print the build identity and exit")

	queues := fs.String("queues", "push,pull", "task kinds this worker takes")
	concurrency := fs.Int("concurrency", 1, "runners in this process")
	name := fs.String("name", "", "identifier recorded on leases (default: the hostname)")
	configFile := fs.String("config", "", "configuration file (see docs/CONFIGURATION.md)")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *showVersion {
		fmt.Println("nit-worker", buildinfo.Get())
		return nil
	}

	kinds, err := parseKinds(*queues)
	if err != nil {
		return err
	}

	cfg, err := nitd.Load(*configFile)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return nitd.Work(ctx, cfg, nitd.WorkerOptions{
		Name:        *name,
		Concurrency: *concurrency,
		Kinds:       kinds,
	}, nitd.WorkerDeps{})
}

// parseKinds turns the -queues flag into the restriction the store applies.
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
