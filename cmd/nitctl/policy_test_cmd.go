package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/NitScm/nit/pkg/policy"
	policyconfig "github.com/NitScm/nit/pkg/policy/config"
)

// policyTest checks a bundle against what somebody wrote down that it should
// do.
//
// `validate` says a bundle is well-formed and `diff` says what a change does to
// people. Neither protects a rule from being deleted: the bundle still
// compiles, every other rule still works, and nothing looks wrong until
// somebody reads something they should not have.
func policyTest(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: nitctl policy test <bundle-dir> <expectations-file> [flags]\n\n" +
			"The expectations file goes beside the bundle, never inside it: the\n" +
			"bundle's version is a hash of every YAML in its directory, and it is\n" +
			"stamped on every decision")
	}

	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable")
	quiet := fs.Bool("quiet", false, "say nothing when everything holds")

	if err := fs.Parse(args[2:]); err != nil {
		return err
	}

	p, err := policyconfig.Load(args[0])
	if err != nil {
		return err
	}

	expectations, err := policyconfig.LoadExpectations(args[0], args[1])
	if err != nil {
		return err
	}

	// The error is separate from the failures on purpose: one means the policy
	// is wrong, the other means the test is. An expectation naming a deleted
	// user is a test that has stopped testing, and reporting it as a pass is
	// how a file of assertions rots into decoration.
	result, err := policy.Check(p, expectations)
	if err != nil {
		return err
	}

	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")

		if err := enc.Encode(result); err != nil {
			return err
		}
	} else {
		reportChecks(result, *quiet)
	}

	if len(result.Failures) > 0 {
		os.Exit(1)
	}

	return nil
}

func reportChecks(result policy.Result, quiet bool) {
	for _, f := range result.Failures {
		fmt.Println(f)
	}

	if len(result.Failures) > 0 {
		fmt.Printf("\n%d of %d checks did not hold.\n", len(result.Failures), result.Checked)
	} else if !quiet {
		fmt.Printf("%d checks hold.\n", result.Checked)
	}

	// Said even on a green run, because this is the failure mode of a file of
	// denial assertions: everything is denied by default, so they keep passing
	// after somebody deletes the rules they were written to protect.
	if len(result.Hollow) == 0 {
		return
	}

	fmt.Printf("\n%d held because everything is denied by default, not because of any rule:\n",
		len(result.Hollow))

	for _, h := range result.Hollow {
		fmt.Printf("  %s: %s %s %s:%s\n", h.Expectation, h.User, h.Action, h.Repository, h.Path)
	}

	fmt.Println()
	fmt.Println("These would still pass with every deny rule in the bundle deleted.")
	fmt.Println("Add `rule: <id>` to assert which rule is supposed to produce the denial.")
}
