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
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/NitScm/nit/internal/buildinfo"
	"github.com/NitScm/nit/pkg/nitd"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "nitd:", err)
		os.Exit(1)
	}
}

// run is deliberately thin. Everything it used to do lives in pkg/nitd, which
// is what an out-of-tree assembly calls — and the reason this binary calls it
// too is that a façade only used by outsiders drifts from what nit does within
// a release.
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

	cfg, err := nitd.Load(*configFile)
	if err != nil {
		return err
	}

	// Signals are trapped before anything long-running starts, so a Ctrl-C
	// during start-up is a clean exit rather than a half-initialized process.
	// pkg/nitd deliberately does not do this: a process embedding nit
	// alongside other things has its own idea of what a signal means.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return nitd.Serve(ctx, cfg, nitd.Deps{})
}
