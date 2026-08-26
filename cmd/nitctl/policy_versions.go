package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/NitScm/nit/internal/store/connect"
	"github.com/NitScm/nit/pkg/policy"
	"github.com/NitScm/nit/pkg/store"
)

// policyVersions lists which bundles have been in force.
//
// Every decision carries a policy version. This is the way back from one: given
// `sha256:a3f1…` out of an audit record, when was it loaded, and — if anybody
// said — which commit produced it.
func policyVersions(args []string) error {
	fs := flag.NewFlagSet("versions", flag.ContinueOnError)
	dsn := fs.String("dsn", "", "the deployment's database")
	tenant := fs.String("tenant", string(policy.DefaultTenant), "which tenant")
	version := fs.String("version", "", "resolve one version rather than listing")
	limit := fs.Int("limit", 30, "how many to list")

	if err := fs.Parse(args); err != nil {
		return err
	}

	st, err := openStore(*dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	ctx := context.Background()
	versions := st.PolicyVersions()

	if *version != "" {
		v, err := versions.ByVersion(ctx, policy.TenantID(*tenant), *version)

		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("this deployment has never loaded %s. "+
				"Either it came from somewhere else, or it predates this table", *version)
		}

		if err != nil {
			return err
		}

		describeVersion(v)

		return nil
	}

	listed, err := versions.List(ctx, policy.TenantID(*tenant), *limit)
	if err != nil {
		return err
	}

	if len(listed) == 0 {
		fmt.Println("no bundle has been recorded for this tenant yet")

		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	defer w.Flush()

	fmt.Fprintln(w, "VERSION\tIN FORCE FROM\tLAST SEEN\tCOMMIT")

	for _, v := range listed {
		commit := v.Commit
		if commit == "" {
			commit = "—"
		}

		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", v.Version,
			v.FirstLoadedAt.UTC().Format("2006-01-02 15:04"),
			v.LastLoadedAt.UTC().Format("2006-01-02 15:04"), commit)
	}

	return nil
}

func describeVersion(v *store.PolicyVersion) {
	fmt.Printf("%s\n", v.Version)
	fmt.Printf("  first loaded  %s\n", v.FirstLoadedAt.UTC().Format("2006-01-02 15:04:05 MST"))
	fmt.Printf("  last loaded   %s\n", v.LastLoadedAt.UTC().Format("2006-01-02 15:04:05 MST"))
	fmt.Printf("  source        %s\n", v.Source)

	if v.Commit == "" {
		fmt.Println()
		fmt.Println("Nobody recorded which commit produced this bundle, so the rules")
		fmt.Println("behind it have to be found by hashing candidates. `nitctl policy")
		fmt.Println("record` is what CI runs to stop that happening again.")

		return
	}

	fmt.Printf("  ref           %s\n", v.Ref)
	fmt.Printf("  commit        %s\n", v.Commit)
	fmt.Println()
	fmt.Println("Check that commit out and the bundle in it is the one that produced")
	fmt.Println("every decision stamped with this version.")
}

// policyRecord attaches provenance to a version this deployment has loaded.
//
// For CI to run after publishing a bundle. The server knows the version and the
// moment; it does not know the commit, because a bundle may arrive as a
// directory, over a seam, or from somewhere with no git in it at all.
func policyRecord(args []string) error {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	dsn := fs.String("dsn", "", "the deployment's database")
	tenant := fs.String("tenant", string(policy.DefaultTenant), "which tenant")
	version := fs.String("version", "", "the bundle version, as `nitctl policy show` prints it")
	ref := fs.String("ref", "", "the ref it was published from")
	commit := fs.String("commit", "", "the commit that produced it")

	if err := fs.Parse(args); err != nil {
		return err
	}

	if *version == "" || *commit == "" {
		return errors.New("-version and -commit are required")
	}

	st, err := openStore(*dsn)
	if err != nil {
		return err
	}
	defer st.Close()

	err = st.PolicyVersions().Attach(context.Background(),
		policy.TenantID(*tenant), *version, *ref, *commit)

	if errors.Is(err, store.ErrNotFound) {
		// Refused rather than inserted: a pairing for a bundle this deployment
		// has never loaded is a claim about something that never happened here.
		return fmt.Errorf("this deployment has never loaded %s, so there is nothing "+
			"to attach a commit to. Has the bundle been rolled out yet?", *version)
	}

	if err != nil {
		return err
	}

	fmt.Printf("%s came from %s\n", *version, *commit)

	return nil
}

// openStore connects to whichever backend the DSN names.
func openStore(dsn string) (store.Store, error) {
	if dsn == "" {
		return nil, errors.New("-dsn is required")
	}

	st, err := connect.Open(context.Background(), dsn)
	if err != nil {
		return nil, err
	}

	return st, nil
}
