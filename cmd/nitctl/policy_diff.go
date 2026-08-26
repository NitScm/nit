package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/NitScm/nit/pkg/policy"
	policyconfig "github.com/NitScm/nit/pkg/policy/config"
)

// policyDiff reports what a change to a bundle does to people.
//
// # Why a command and not a code review
//
// A diff of the YAML says four lines changed. It does not say that one of them
// put twelve people into a group that reads `config/**`, because the group is
// in another file and the rule granting it was not touched. Past the size where
// one person knows every rule, approving a permission change by reading its
// diff is a signature rather than a review.
//
// It is meant to run in CI on a pull request, with -exit-code, so that a change
// which widens somebody's access cannot merge without a human having seen a
// list of who and to what.
func policyDiff(args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)

	asJSON := fs.Bool("json", false, "machine-readable, for posting on a pull request")
	exitCode := fs.Bool("exit-code", false,
		"exit 1 if anything changed, as `git diff --exit-code` does")
	wideningOnly := fs.Bool("widening", false,
		"only what gives somebody more; the narrowing half is rarely the risk")

	if len(args) < 2 {
		return fmt.Errorf("usage: nitctl policy diff <before-dir> <after-dir> [flags]")
	}

	before, err := policyconfig.Load(args[0])
	if err != nil {
		return fmt.Errorf("before: %w", err)
	}

	after, err := policyconfig.Load(args[1])
	if err != nil {
		return fmt.Errorf("after: %w", err)
	}

	if err := fs.Parse(args[2:]); err != nil {
		return err
	}

	diff := policy.Compare(before, after)

	changes := diff.Changes
	if *wideningOnly {
		changes = diff.Widening()
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		if err := enc.Encode(map[string]any{
			"users_added":   diff.UsersAdded,
			"users_removed": diff.UsersRemoved,
			"repos_added":   diff.ReposAdded,
			"repos_removed": diff.ReposRemoved,
			"changes":       changes,
		}); err != nil {
			return err
		}
	} else {
		reportDiff(diff, changes)
	}

	if *exitCode && !diff.Empty() {
		os.Exit(1)
	}

	return nil
}

func reportDiff(diff policy.Diff, changes []policy.Change) {
	if diff.Empty() {
		fmt.Println("Nothing changed for anybody.")
		fmt.Println()
		fmt.Println("Rules can be reordered, split or regrouped freely: the engine")
		fmt.Println("considers every matching rule and deny always wins, so order")
		fmt.Println("carries no meaning to change.")

		return
	}

	for _, id := range diff.UsersAdded {
		fmt.Printf("+ person   %s\n", id)
	}

	for _, id := range diff.UsersRemoved {
		fmt.Printf("- person   %s\n", id)
	}

	for _, id := range diff.ReposAdded {
		fmt.Printf("+ repo     %s\n", id)
	}

	for _, id := range diff.ReposRemoved {
		fmt.Printf("- repo     %s\n", id)
	}

	widening := 0

	for _, change := range changes {
		if change.Widens() {
			widening++
		}

		fmt.Printf("\n%s\n", change.User)

		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)

		// Widening first, always. It is what a reviewer is looking for, and a
		// list that buries it under twenty narrowing lines is a list that gets
		// skimmed.
		for _, g := range change.Allowed {
			line(w, "  now allowed", g)
		}

		for _, g := range change.NoLongerDenied {
			line(w, "  DENY REMOVED", g)
		}

		for _, g := range change.Denied {
			line(w, "  now denied", g)
		}

		for _, g := range change.NoLongerAllowed {
			line(w, "  no longer allowed", g)
		}

		w.Flush()
	}

	fmt.Println()

	if widening > 0 {
		fmt.Printf("%d %s can reach more than before.\n", widening, people(widening))
	} else {
		fmt.Println("Nobody can reach more than before.")
	}

	// Said every time, because it is the limit of what this can tell anybody.
	fmt.Println()
	fmt.Println("This compares rules as they apply to people, not outcomes at every")
	fmt.Println("path — paths are infinite and any sample would be a sample. For one")
	fmt.Println("path and one person, `nitctl policy explain` answers exactly.")
}

func line(w *tabwriter.Writer, label string, g policy.Grant) {
	where := g.Path
	if g.Ref != "" {
		where += "\ton " + g.Ref
	} else {
		where += "\t"
	}

	via := ""
	if g.Via != "" {
		via = "via " + g.Via
	}

	fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", label, g.Action, g.Repository, where, g.RuleID, via)
}

func people(n int) string {
	if n == 1 {
		return "person"
	}

	return "people"
}
