package enforce

import (
	"fmt"

	"github.com/NitScm/nit/pkg/patch"
	"github.com/NitScm/nit/pkg/policy"
)

// GuardKind names the structural protection that required a check, when the
// check does not come from an ordinary path rule.
type GuardKind string

const (
	// GuardNone marks an ordinary path/action check.
	GuardNone GuardKind = ""

	// GuardSymlink: the change creates or turns a path into a symlink. A
	// symlink is a read of its target performed by whoever resolves it, so
	// introducing one is an authorization decision, not a content edit.
	GuardSymlink GuardKind = "symlink"

	// GuardSubmodule: the change adds or moves a submodule pointer, which
	// injects content from outside the repository entirely.
	GuardSubmodule GuardKind = "submodule"

	// GuardProtectedPath: the change touches a path that governs how the
	// repository is processed — CI definitions, .gitattributes, .gitmodules.
	// Write access to those is read access to everything the CI can see.
	GuardProtectedPath GuardKind = "protected_path"
)

// Check is one authorization question asked about one change, and its answer.
// The list of checks is what makes a verdict explainable, and it is what the
// audit log records.
type Check struct {
	Path   string
	Action policy.Action
	Guard  GuardKind

	Decision policy.Decision
}

// Passed reports whether the check authorized the operation.
func (c Check) Passed() bool { return c.Decision.Allowed }

// String renders the check for CLI output and logs.
func (c Check) String() string {
	if c.Guard != GuardNone {
		return fmt.Sprintf("%s [%s] %s: %s", c.Path, c.Guard, c.Action, c.Decision)
	}
	return fmt.Sprintf("%s %s: %s", c.Path, c.Action, c.Decision)
}

// Verdict is the outcome for a single patch section.
type Verdict struct {
	Change *patch.Change

	// Checks holds every question asked about the section, in the order they
	// were asked, whether they passed or not.
	Checks []Check

	// Allowed is true when every check passed.
	Allowed bool
}

// Denials returns the checks that refused the section.
func (v Verdict) Denials() []Check {
	var out []Check
	for _, c := range v.Checks {
		if !c.Passed() {
			out = append(out, c)
		}
	}
	return out
}

// requirement is a (path, action) pair that a change needs in order to be
// authorized.
type requirement struct {
	path   string
	action policy.Action
	guard  GuardKind
}

// requirementsFor returns everything a change must be authorized for in the
// write direction.
//
// The mapping is deliberately finer than "write": deleting a file is not
// modifying it, and creating one is not either. A rename is the only operation
// that spans two paths, and both sides must hold — otherwise a rename becomes a
// way to move a file out of a protected subtree, or into one.
func requirementsFor(c *patch.Change) []requirement {
	var reqs []requirement

	switch c.Op {
	case patch.OpAdd:
		reqs = append(reqs, requirement{c.NewPath, policy.ActionCreate, GuardNone})

	case patch.OpModify:
		reqs = append(reqs,
			requirement{c.NewPath, policy.ActionRead, GuardNone},
			requirement{c.NewPath, policy.ActionWrite, GuardNone},
		)

	case patch.OpDelete:
		reqs = append(reqs,
			requirement{c.OldPath, policy.ActionRead, GuardNone},
			requirement{c.OldPath, policy.ActionDelete, GuardNone},
		)

	case patch.OpRename:
		reqs = append(reqs,
			requirement{c.OldPath, policy.ActionRead, GuardNone},
			requirement{c.OldPath, policy.ActionDelete, GuardNone},
			requirement{c.NewPath, policy.ActionCreate, GuardNone},
		)

	case patch.OpCopy:
		reqs = append(reqs,
			requirement{c.OldPath, policy.ActionRead, GuardNone},
			requirement{c.NewPath, policy.ActionCreate, GuardNone},
		)
	}

	return reqs
}
