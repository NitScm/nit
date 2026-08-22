// Command nit is the developer CLI.
//
// It never talks to the forge. It produces a patch against its sync point,
// hands it to the control plane, and applies whatever filtered patch comes
// back; the upstream credentials live on the server and a developer machine
// never holds them.
//
//	nit login <server-url>              store a token for a server
//	nit clone <repository> [directory]  create a workspace from a filtered snapshot
//	nit pull                            fetch and apply the filtered upstream diff
//	nit push -m <message>               submit the local changes
//	nit push --check                    authorize without submitting anything
//	nit status                          show the workspace and its sync point
//	nit whoami                          show the authenticated identity
//
// The command layer uses the standard library's flag package. The surface is
// small and stable enough not to need more; Cobra would buy shell completion,
// and swapping to it touches only this file.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/NitScm/nit/internal/client"
	"github.com/NitScm/nit/internal/flow"
	"github.com/NitScm/nit/internal/workspace"
	"github.com/NitScm/nit/pkg/gitx"
	"github.com/NitScm/nit/pkg/protocol"
)

const usage = `nit - authorized git workflow

Usage:
  nit login <server-url> [-token <token>]
  nit clone <repository> [directory] [-branch <branch>] [-server <url>]
  nit pull
  nit push -m <message> [--check] [--drop-unauthorized]
  nit status
  nit whoami [-server <url>]

Run "nit <command> -h" for the options of a command.
`

func main() {
	// Ctrl-C must not leave a half-applied patch behind: the context reaches
	// every git call and every request, and the flows are ordered so an
	// interruption never records a sync point for work that did not land.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, os.Args[1:]); err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}

		fmt.Fprintln(os.Stderr, "nit:", format(err))
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return errors.New("no command")
	}

	switch args[0] {
	case "login":
		return login(ctx, args[1:])
	case "clone":
		return clone(ctx, args[1:])
	case "pull":
		return pull(ctx, args[1:])
	case "push":
		return push(ctx, args[1:])
	case "status":
		return status(ctx, args[1:])
	case "whoami":
		return whoami(ctx, args[1:])
	case "help", "-h", "--help":
		fmt.Print(usage)
		return nil
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

// format renders an error for a developer.
//
// A server denial carries the rule that refused it and the rule author's
// message; printing only "forbidden" would turn every denial into a support
// ticket, which is the failure mode this whole product has to avoid.
func format(err error) string {
	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		return err.Error()
	}

	message := apiErr.Message

	if apiErr.Code == protocol.CodeUnauthorizedPaths && len(apiErr.Denials) > 0 {
		message += "\n"

		for _, denial := range apiErr.Denials {
			message += fmt.Sprintf("\n  %s (%s)", denial.Path, denial.Action)

			if denial.RuleID != "" {
				message += fmt.Sprintf("\n      refused by rule %s", denial.RuleID)
			}
			if denial.Guard != "" {
				message += fmt.Sprintf("\n      guard: %s", denial.Guard)
			}
			if denial.Description != "" {
				message += fmt.Sprintf("\n      %s", denial.Description)
			}
		}
	}

	return message
}

// reporter prints progress to stderr, leaving stdout for results so output
// stays pipeable.
type reporter struct{}

func (reporter) Progress(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "  "+format+"\n", args...)
}

// openWorkspace loads the checkout in the current directory and a client
// authenticated for its server.
func openWorkspace(ctx context.Context) (*workspace.Workspace, *client.Client, error) {
	dir, err := os.Getwd()
	if err != nil {
		return nil, nil, err
	}

	ws, err := workspace.Open(ctx, gitx.NewExecGit(), dir)
	if err != nil {
		return nil, nil, err
	}

	c, err := clientFor(ws.State.Server)
	if err != nil {
		return nil, nil, err
	}

	return ws, c, nil
}

// clientFor builds a client from the stored credentials.
func clientFor(server string) (*client.Client, error) {
	if server == "" {
		return nil, errors.New("no server configured")
	}

	creds, err := workspace.LoadCredentials()
	if err != nil {
		return nil, err
	}

	token, err := creds.Token(server)
	if err != nil {
		return nil, err
	}

	return client.New(server, token), nil
}

func newRunner(c *client.Client) *flow.Runner {
	return &flow.Runner{
		Client:   c,
		Git:      gitx.NewExecGit(),
		Reporter: reporter{},
	}
}
