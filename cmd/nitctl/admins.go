package main

import (
	"context"
	"flag"
	"fmt"
	"strings"

	"github.com/NitScm/nit/internal/bootstrap"
	"github.com/NitScm/nit/internal/store/connect"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// admins reads and writes the groups that may operate a tenant.
//
// A database command rather than an API one, like `migrate` and `token`: it
// decides who may use the operations API, so putting it *behind* the operations
// API would make an operator locked out unable to unlock themselves.
//
// The list stays outside the customer's policy bundle (D28) — the console is
// the tool for diagnosing a broken bundle, so the permission to use it cannot
// live in the thing it exists to debug. The group names come from their bundle,
// because that is where groups are defined; which of them may operate is
// decided here.
func admins(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: nitctl admins list|set [-tenant T] [-groups a,b]")
	}

	fs := flag.NewFlagSet("admins", flag.ContinueOnError)

	tenant := fs.String("tenant", string(policy.DefaultTenant), "which tenant")
	groups := fs.String("groups", "", "comma-separated group ids (set)")
	dsn := fs.String("dsn", "", "database DSN (defaults to the configured database.url)")
	configFile := fs.String("config", "", "configuration file to read the DSN from")

	if err := fs.Parse(args[1:]); err != nil {
		return err
	}

	resolved := *dsn
	if resolved == "" {
		cfg, err := bootstrap.LoadConfigFrom(*configFile)
		if err == nil {
			resolved = cfg.DatabaseURL
		}
	}
	if resolved == "" {
		return fmt.Errorf("-dsn is required (or set database.url, or %s)", bootstrap.EnvDatabaseURL)
	}

	ctx := context.Background()

	st, err := connect.Open(ctx, resolved)
	if err != nil {
		return err
	}
	defer st.Close()

	// The tenant is stamped so a backend enforcing row-level security lets the
	// write through. An operator naming a tenant on the command line is the one
	// caller entitled to say which.
	ctx = store.WithTenant(ctx, policy.TenantID(*tenant))

	switch args[0] {
	case "list":
		return listAdmins(ctx, st, policy.TenantID(*tenant))
	case "set":
		return setAdmins(ctx, st, policy.TenantID(*tenant), *groups)
	default:
		return fmt.Errorf("unknown admins subcommand %q", args[0])
	}
}

func listAdmins(ctx context.Context, st store.Store, tenant policy.TenantID) error {
	groups, err := st.Tenants().AdminGroups(ctx, tenant)
	if err != nil {
		return err
	}

	if len(groups) == 0 {
		fmt.Printf("no operator groups set for %s\n", tenant)
		fmt.Println("the deployment-wide server.admin_groups decides.")

		return nil
	}

	fmt.Printf("operator groups for %s:\n", tenant)

	for _, group := range groups {
		fmt.Printf("  %s\n", group)
	}

	return nil
}

func setAdmins(ctx context.Context, st store.Store, tenant policy.TenantID, spec string) error {
	var groups []policy.GroupID

	for part := range strings.SplitSeq(spec, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			groups = append(groups, policy.GroupID(trimmed))
		}
	}

	if err := st.Tenants().SetAdminGroups(ctx, tenant, groups); err != nil {
		return err
	}

	if len(groups) == 0 {
		// Emptying is a real operation and worth confirming out loud: it does
		// not lock everybody out, it hands the decision back to the
		// deployment-wide configuration.
		fmt.Printf("cleared the operator groups for %s; server.admin_groups decides again\n", tenant)

		return nil
	}

	fmt.Printf("operator groups for %s: %s\n", tenant, spec)

	return nil
}
