// Command nitctl is the operator console: it validates policy bundles,
// explains decisions, and (once the control plane exists) inspects tasks,
// queues and audit records through the same API the web UI will use.
//
// Everything it does goes through the public API rather than the database, so
// that the API is exercised from day one and the future Angular front end has
// no capability nitctl lacks.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/NitScm/nit/internal/auth"
	"github.com/NitScm/nit/internal/bootstrap"
	"github.com/NitScm/nit/internal/buildinfo"
	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/internal/store/postgres"
	"github.com/NitScm/nit/migrations"
	"github.com/NitScm/nit/pkg/policy"
	policyconfig "github.com/NitScm/nit/pkg/policy/config"
	"github.com/NitScm/nit/pkg/store"
)

const usage = `nitctl - nit operator console

Usage:
  nitctl policy validate <bundle-dir>
  nitctl policy explain  <bundle-dir> -repo R -user U -path P [-action A] [-ref REF]
  nitctl policy show     <bundle-dir>
  nitctl config show     [-config FILE]
  nitctl config path     [-config FILE]
  nitctl config init     [-config FILE] [-force]
  nitctl migrate         -dsn <postgres-dsn> [-status]
  nitctl token create    -user U [-label L] [-ttl 720h]
  nitctl token list      -user U
  nitctl token revoke    -id ID
  nitctl stats           [-server URL] [-token T] [-json]
  nitctl tasks           [-state S] [-kind K] [-repository R] [-limit N] [-json]
  nitctl audit           [-user U] [-repository R] [-request ID] [-since 24h] [-json]
  nitctl version

Commands:
  config            Inspect the effective configuration, or write a starter file.
  policy validate   Compile a bundle and report every problem it contains.
  policy explain    Show the decision for one subject, path and action.
  policy show       List the repositories, users and groups of a bundle.
  migrate           Apply pending schema migrations.
  token             Issue, list and revoke CLI tokens.
  stats             Queue depth, task counts and recent denials.
  tasks             List queued, running and finished tasks.
  audit             Query the audit log: who did what, when, under which rule.

Token commands read NIT_DATABASE_URL and NIT_POLICY_DIR from the environment,
or take -dsn and -policy.
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "nitctl:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("no command")
	}

	switch args[0] {
	case "version", "-version", "--version":
		fmt.Println("nitctl", buildinfo.Get())
		return nil
	}

	if args[0] == "config" {
		return configCommand(args[1:])
	}

	if args[0] == "migrate" {
		return migrate(args[1:])
	}

	if args[0] == "token" {
		return token(args[1:])
	}

	// The operations commands read the API rather than the database, so nitctl
	// and the web console exercise exactly the same endpoints.
	switch args[0] {
	case "stats", "tasks", "audit":
		return ops(args[0], args[1:])
	}

	if len(args) < 2 || args[0] != "policy" {
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown command")
	}

	switch args[1] {
	case "validate":
		return policyValidate(args[2:])
	case "explain":
		return policyExplain(args[2:])
	case "show":
		return policyShow(args[2:])
	default:
		fmt.Fprint(os.Stderr, usage)
		return fmt.Errorf("unknown policy subcommand %q", args[1])
	}
}

func policyValidate(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: nitctl policy validate <bundle-dir>")
	}

	p, err := policyconfig.Load(args[0])
	if err != nil {
		return err
	}

	fmt.Printf("bundle ok\n")
	fmt.Printf("  version:      %s\n", p.Version())
	fmt.Printf("  repositories: %d\n", len(p.Repositories()))

	return nil
}

func policyShow(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: nitctl policy show <bundle-dir>")
	}

	p, err := policyconfig.Load(args[0])
	if err != nil {
		return err
	}

	fmt.Printf("version: %s\n\n", p.Version())

	for _, repo := range p.Repositories() {
		fmt.Printf("repository %s\n", repo.ID)
		fmt.Printf("  remote: %s (%s)\n", repo.Remote, repo.Forge)
		fmt.Printf("  branch: %s\n", repo.DefaultBranch)

		for _, rule := range p.Rules(repo.ID) {
			paths := make([]string, 0, len(rule.Paths))
			for _, pat := range rule.Paths {
				paths = append(paths, pat.String())
			}

			actions := make([]string, 0, len(rule.Actions))
			for _, a := range rule.Actions {
				actions = append(actions, string(a))
			}

			fmt.Printf("  %-6s %-28s %-22s %s\n",
				rule.Effect, rule.ID, rule.Subject, strings.Join(paths, ","))
			fmt.Printf("         actions: %s\n", strings.Join(actions, ","))
		}

		fmt.Println()
	}

	return nil
}

func policyExplain(args []string) error {
	// The bundle directory comes first, then the flags: Go's flag package stops
	// parsing at the first non-flag argument, so a positional argument placed
	// after the flags would be silently ignored.
	if len(args) == 0 {
		return fmt.Errorf("usage: nitctl policy explain <bundle-dir> -repo R -user U -path P")
	}

	dir := args[0]

	fs := flag.NewFlagSet("explain", flag.ContinueOnError)

	repo := fs.String("repo", "", "repository id")
	user := fs.String("user", "", "user id")
	path := fs.String("path", "", "repository-relative path")
	ref := fs.String("ref", "", "fully qualified ref, e.g. refs/heads/main")
	action := fs.String("action", "", "action; empty means every action")

	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *repo == "" || *user == "" || *path == "" {
		return fmt.Errorf("-repo, -user and -path are required")
	}

	p, err := policyconfig.Load(dir)
	if err != nil {
		return err
	}

	subject, err := p.Subject(policy.UserID(*user))
	if err != nil {
		return err
	}

	actions := policy.AllActions
	if *action != "" {
		a := policy.Action(*action)
		if !a.Valid() {
			return fmt.Errorf("unknown action %q", *action)
		}
		actions = []policy.Action{a}
	}

	fmt.Printf("%s on %s (%s)\n", *user, *path, *repo)
	if len(subject.Groups) > 0 {
		groups := make([]string, 0, len(subject.Groups))
		for _, g := range subject.Groups {
			groups = append(groups, string(g))
		}
		fmt.Printf("groups: %s\n", strings.Join(groups, ", "))
	}
	fmt.Println()

	for _, a := range actions {
		d := p.Evaluate(policy.Request{
			Repo:    policy.RepoID(*repo),
			Ref:     *ref,
			Subject: subject,
			Path:    *path,
			Action:  a,
		})

		mark := "DENY "
		if d.Allowed {
			mark = "ALLOW"
		}

		fmt.Printf("  %s %-7s %s\n", mark, a, d)

		if d.Description != "" {
			fmt.Printf("          %s\n", d.Description)
		}
	}

	return nil
}

// migrate applies pending schema migrations.
//
// It is a separate command rather than something the server does at start-up:
// a schema change is a deployment step an operator decides to take, and a
// server that migrates on boot will happily run a half-rolled-out DDL across
// several replicas at once.
func migrate(args []string) error {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)

	dsn := fs.String("dsn", "", "PostgreSQL DSN (defaults to the configured database.url)")
	status := fs.Bool("status", false, "list migrations without applying anything")

	if err := fs.Parse(args); err != nil {
		return err
	}

	loaded, err := postgres.LoadMigrations(migrations.FS)
	if err != nil {
		return err
	}

	if *status {
		for _, m := range loaded {
			fmt.Printf("  %04d  %s\n", m.Version, m.Name)
		}
		return nil
	}

	resolved := *dsn
	if resolved == "" {
		// Fall back to the same configuration nitd reads, so an operator does
		// not have to restate the DSN their configuration file already holds.
		cfg, err := bootstrap.LoadConfigFrom("")
		if err == nil {
			resolved = cfg.DatabaseURL
		}
	}
	if resolved == "" {
		return fmt.Errorf("-dsn is required (or set database.url, or %s)", bootstrap.EnvDatabaseURL)
	}

	ctx := context.Background()

	s, err := postgres.Open(ctx, resolved)
	if err != nil {
		return err
	}
	defer s.Close()

	applied, err := postgres.Migrate(ctx, s.Pool(), loaded)
	if err != nil {
		return err
	}

	if applied == 0 {
		fmt.Println("schema up to date")
		return nil
	}

	fmt.Printf("applied %d migration(s)\n", applied)
	return nil
}

// ---------------------------------------------------------------------------
// tokens
// ---------------------------------------------------------------------------

// token issues, lists and revokes the credentials the CLI authenticates with.
//
// Issuing is an operator action rather than a self-service login flow. A device
// flow against the forge is the obvious next step; until then, an administrator
// hands out a token and the trust chain stays short enough to reason about.
func token(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: nitctl token create|list|revoke")
	}

	fs := flag.NewFlagSet("token", flag.ContinueOnError)

	dsn := fs.String("dsn", "", "PostgreSQL DSN (defaults to the configured database.url)")
	policyDir := fs.String("policy", "", "policy bundle directory (defaults to the configured policy.dir)")
	user := fs.String("user", "", "policy user id")
	label := fs.String("label", "", "free-text label, typically a machine name")
	id := fs.String("id", "", "session id (revoke)")
	ttl := fs.Duration("ttl", 720*time.Hour, "how long the token stays valid")

	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	resolvedDSN, resolvedPolicy := *dsn, *policyDir

	if resolvedDSN == "" || resolvedPolicy == "" {
		cfg, err := bootstrap.LoadConfigFrom("")
		if err == nil {
			if resolvedDSN == "" {
				resolvedDSN = cfg.DatabaseURL
			}
			if resolvedPolicy == "" {
				resolvedPolicy = cfg.PolicyDir
			}
		}
	}

	if resolvedDSN == "" {
		return fmt.Errorf("-dsn is required (or set database.url, or %s)", bootstrap.EnvDatabaseURL)
	}
	if resolvedPolicy == "" {
		return fmt.Errorf("-policy is required (or set policy.dir, or %s)", bootstrap.EnvPolicyDir)
	}

	ctx := context.Background()

	compiled, err := policyconfig.Load(resolvedPolicy)
	if err != nil {
		return err
	}

	st, err := postgres.Open(ctx, resolvedDSN)
	if err != nil {
		return err
	}
	defer st.Close()

	service := auth.NewService(st, policyloader.NewStatic(compiled), policy.DefaultTenant, nil)

	switch args[0] {
	case "create":
		if *user == "" {
			return fmt.Errorf("-user is required")
		}

		// The bundle is the source of truth: a token can only be issued to
		// someone it declares, so a typo produces an error rather than a
		// credential for an account that authorizes nothing.
		record, err := bootstrap.ReconcileUser(ctx, st, compiled, policy.DefaultTenant, policy.UserID(*user))
		if err != nil {
			return err
		}

		plaintext, session, err := service.Issue(ctx, record.ID, *label, *ttl)
		if err != nil {
			return err
		}

		fmt.Printf("session: %s\n", session.ID)
		fmt.Printf("user:    %s\n", *user)
		if session.ExpiresAt != nil {
			fmt.Printf("expires: %s\n", session.ExpiresAt.Format(time.RFC3339))
		}
		fmt.Printf("\n%s\n\n", plaintext)
		fmt.Println("This token is shown once and is not recoverable. Store it with:")
		fmt.Println("  nit login <server-url>")

		return nil

	case "list":
		if *user == "" {
			return fmt.Errorf("-user is required")
		}

		record, err := st.Users().ByPolicyID(ctx, policy.DefaultTenant, policy.UserID(*user))
		if err != nil {
			return err
		}

		sessions, err := st.Sessions().ListByUser(ctx, record.ID)
		if err != nil {
			return err
		}

		if len(sessions) == 0 {
			fmt.Println("no tokens")
			return nil
		}

		fmt.Printf("%-38s %-16s %-22s %s\n", "SESSION", "LABEL", "EXPIRES", "STATE")

		now := time.Now().UTC()

		for _, s := range sessions {
			expires := "never"
			if s.ExpiresAt != nil {
				expires = s.ExpiresAt.Format(time.RFC3339)
			}

			state := "active"
			switch {
			case s.RevokedAt != nil:
				state = "revoked"
			case !s.Active(now):
				state = "expired"
			}

			fmt.Printf("%-38s %-16s %-22s %s\n", s.ID, s.Label, expires, state)
		}

		return nil

	case "revoke":
		if *id == "" {
			return fmt.Errorf("-id is required")
		}
		if err := service.Revoke(ctx, store.ID(*id)); err != nil {
			return err
		}

		fmt.Println("revoked")
		return nil

	default:
		return fmt.Errorf("unknown token subcommand %q", args[0])
	}
}
