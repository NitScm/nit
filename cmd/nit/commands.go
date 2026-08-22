package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/NitScm/nit/internal/client"
	"github.com/NitScm/nit/internal/flow"
	"github.com/NitScm/nit/internal/workspace"
	"github.com/NitScm/nit/pkg/protocol"
)

// login stores a token for a server after checking that it works.
//
// The token is verified before being written. Storing an unverified credential
// means the failure surfaces later, during a push, where it is indistinguishable
// from half a dozen other problems.
func login(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	token := fs.String("token", "", "token to store; read from stdin when omitted")

	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("usage: nit login <server-url>")
	}

	server := strings.TrimRight(rest[0], "/")

	probe := client.New(server, "")

	health, err := probe.Health(ctx)
	if err != nil {
		return fmt.Errorf("%s does not look like a nit server: %w", server, err)
	}
	if health.ProtocolVersion != protocol.Version {
		return fmt.Errorf("the server speaks protocol %s, this CLI speaks %s; upgrade one of them",
			health.ProtocolVersion, protocol.Version)
	}

	value := *token

	if value == "" {
		fmt.Fprintf(os.Stderr, "Token for %s: ", server)

		scanner := bufio.NewScanner(os.Stdin)
		if !scanner.Scan() {
			return errors.New("no token supplied")
		}

		value = strings.TrimSpace(scanner.Text())
	}

	if value == "" {
		return errors.New("no token supplied")
	}

	who, err := client.New(server, value).WhoAmI(ctx)
	if err != nil {
		return err
	}

	creds, err := workspace.LoadCredentials()
	if err != nil {
		return err
	}

	creds.Set(server, value)

	if err := creds.Save(); err != nil {
		return err
	}

	path, _ := workspace.CredentialsPath()

	fmt.Printf("Logged in to %s as %s\n", server, who.User)
	fmt.Printf("Credential stored in %s\n", path)

	return nil
}

func clone(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("clone", flag.ContinueOnError)

	server := fs.String("server", os.Getenv("NIT_SERVER"), "server URL (defaults to $NIT_SERVER)")
	branch := fs.String("branch", "", "branch to track (defaults to the repository's)")
	label := fs.String("label", "", "label for this workspace, shown to operators")

	rest, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(rest) < 1 || len(rest) > 2 {
		return errors.New("usage: nit clone <repository> [directory]")
	}

	resolved := *server

	if resolved == "" {
		// A single stored credential is unambiguous, so asking for -server
		// would be pure ceremony.
		creds, err := workspace.LoadCredentials()
		if err != nil {
			return err
		}
		if len(creds.Tokens) != 1 {
			return errors.New("-server is required (or set NIT_SERVER)")
		}
		for s := range creds.Tokens {
			resolved = s
		}
	}

	c, err := clientFor(resolved)
	if err != nil {
		return err
	}

	directory := ""
	if len(rest) == 2 {
		directory = rest[1]
	}

	result, err := newRunner(c).Clone(ctx, flow.CloneOptions{
		Server:     resolved,
		Repository: rest[0],
		Branch:     *branch,
		Directory:  directory,
		Label:      *label,
	})
	if err != nil {
		return err
	}

	fmt.Printf("Cloned %s into %s\n", rest[0], result.Directory)
	printPullReport(result.Report)

	return nil
}

func pull(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}

	ws, c, err := openWorkspace(ctx)
	if err != nil {
		return err
	}

	report, err := newRunner(c).Pull(ctx, ws)
	if err != nil {
		return err
	}

	if report.UpstreamCommit == "" {
		fmt.Println("Already up to date")
		return nil
	}

	fmt.Printf("Updated %s@%s to %s\n", ws.State.Repository, ws.State.Branch, shorten(report.UpstreamCommit))
	printPullReport(report)

	return nil
}

func push(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)

	message := fs.String("m", "", "commit message for the change landing upstream")
	check := fs.Bool("check", false, "authorize without submitting anything")
	drop := fs.Bool("drop-unauthorized", false,
		"drop refused files instead of rejecting the push; what lands then differs from what you committed")

	if _, err := parseFlags(fs, args); err != nil {
		return err
	}

	ws, c, err := openWorkspace(ctx)
	if err != nil {
		return err
	}

	result, err := newRunner(c).Push(ctx, ws, flow.PushOptions{
		Message:          *message,
		Check:            *check,
		DropUnauthorized: *drop,
	})
	if err != nil {
		return err
	}

	if *check {
		fmt.Printf("%d file(s) would be pushed, %d refused\n",
			result.Report.FilesAccepted, result.Report.FilesDenied)
		return nil
	}

	fmt.Printf("Pushed %d file(s) to %s@%s as %s\n",
		result.Report.FilesAccepted, ws.State.Repository, ws.State.Branch,
		shorten(result.UpstreamCommit))

	if result.Report.FilesDenied > 0 {
		fmt.Printf("%d file(s) were dropped:\n", result.Report.FilesDenied)

		for _, denial := range result.Report.Denials {
			fmt.Printf("  %s (%s)\n", denial.Path, denial.Reason)
		}
	}

	// The branch moved under the push, or files were dropped: what landed is
	// not what this workspace holds, so it must resynchronize before pushing
	// again.
	if result.NeedsPull {
		fmt.Println("\nYour change landed, but the branch has moved on. Run: nit pull")
	}

	return nil
}

func status(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("status", flag.ContinueOnError)
	if _, err := parseFlags(fs, args); err != nil {
		return err
	}

	ws, _, err := openWorkspace(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("repository: %s\n", ws.State.Repository)
	fmt.Printf("branch:     %s\n", ws.State.Branch)
	fmt.Printf("server:     %s\n", ws.State.Server)
	fmt.Printf("workspace:  %s\n", ws.State.Workspace)

	if ws.State.SyncToken == "" {
		fmt.Println("sync:       never synchronized")
	} else {
		fmt.Printf("sync:       %s\n", shorten(ws.State.LocalBase))
	}

	patch, err := ws.Diff(ctx)
	if err != nil {
		return err
	}

	switch {
	case len(patch) == 0:
		fmt.Println("changes:    nothing to push")
	default:
		fmt.Printf("changes:    %d bytes to push\n", len(patch))
	}

	dirty, err := ws.Dirty(ctx)
	if err != nil {
		return err
	}
	if dirty {
		fmt.Println("            (uncommitted changes are not included)")
	}

	return nil
}

func whoami(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("whoami", flag.ContinueOnError)
	server := fs.String("server", os.Getenv("NIT_SERVER"), "server URL")

	if _, err := parseFlags(fs, args); err != nil {
		return err
	}

	resolved := *server

	// Inside a workspace the server is known; outside it must be named.
	if resolved == "" {
		if ws, _, err := openWorkspace(ctx); err == nil {
			resolved = ws.State.Server
		}
	}

	if resolved == "" {
		creds, err := workspace.LoadCredentials()
		if err != nil {
			return err
		}
		if len(creds.Tokens) != 1 {
			return errors.New("-server is required (or set NIT_SERVER)")
		}
		for s := range creds.Tokens {
			resolved = s
		}
	}

	c, err := clientFor(resolved)
	if err != nil {
		return err
	}

	who, err := c.WhoAmI(ctx)
	if err != nil {
		return err
	}

	fmt.Printf("user:   %s <%s>\n", who.User, who.Email)
	fmt.Printf("groups: %s\n", strings.Join(who.Groups, ", "))
	fmt.Printf("policy: %s\n", who.PolicyVersion)

	return nil
}

// printPullReport says what was delivered and what was withheld.
//
// The count of withheld files is shown without their paths: naming them would
// leak the structure the read rules exist to hide. It is shown at all because a
// developer who does not know something was withheld will mistake a missing
// file for a deleted one.
func printPullReport(report protocol.PullReport) {
	if report.FilesDelivered > 0 {
		fmt.Printf("%d file(s) updated\n", report.FilesDelivered)
	}
	if report.FilesWithheld > 0 {
		fmt.Printf("%d file(s) withheld by policy\n", report.FilesWithheld)
	}
}

func shorten(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}
