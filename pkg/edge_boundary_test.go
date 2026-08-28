package pkg_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The edge is a binary that does not exist yet, and this is what keeps it
// cheap to write.
//
// # What the edge is
//
// Gap 6 of `saas-thinking/03-code-gaps.md`: splitting today's `nitd` into a
// customer-run **edge** — parse the patch, evaluate the bundle, enforce the
// decision, keep the bytes locally — and a hosted **control plane** that holds
// identity, coordination, sync points and the audit trail.
//
// It is the last thing standing between this product and a hosted offering,
// and the reason is stated plainly in the enrolment runbook: authorizing a push
// means decompressing the patch and reading the paths inside it, so `nitd` sees
// the source code and not only the worker. "We host the control plane but not
// the workers" protects nothing. The split is what makes hosting sayable.
//
// # Why this test exists rather than a document saying it is fine
//
// The estimate for that work — months, not years — rests entirely on one claim:
// **the edge is a new binary composed of packages that already exist, not new
// logic.** Every package it needs is listed in the gap, and each one is pure by
// construction.
//
// That claim is true today. It was measured, not assumed: the five packages
// below import nothing from `internal/` at all.
//
// It is also unprotected. Go permits any package in a module to import that
// module's `internal/` tree, so one `import "…/internal/store"` added to
// `pkg/enforce` for a convenient helper would make the edge un-composable —
// and nothing would fail. The build stays green, the tests stay green, and the
// cost of the split silently goes from "compose what exists" to "untangle it
// first". Nobody finds out until somebody sits down to write the binary.
//
// So this fails at the moment the seam is crossed, which is the only moment the
// crossing is cheap to undo.
//
// # Why not simply forbid `pkg` → `internal` everywhere
//
// Because `pkg/nitd` is allowed to, and should be. It assembles and runs a
// server; that is its whole job, it is a leaf nothing else imports, and
// `pkg/nitd/boundary_test.go` holds it to being one. A blanket rule would have
// to carve it out anyway, and a rule with a carve-out teaches less than a list
// of the packages the claim actually depends on.
func TestTheEdgePackagesStayComposable(t *testing.T) {
	// The packages the edge is made of, from the gap's own table. `internal/
	// client` is on that list too and is deliberately absent here: it is
	// already inside `internal/`, so it moves with the edge rather than being
	// something the edge must be able to reach out of `pkg/` to use.
	edge := []string{
		"github.com/NitScm/nit/pkg/patch",   // parse a patch
		"github.com/NitScm/nit/pkg/policy",  // evaluate a bundle
		"github.com/NitScm/nit/pkg/enforce", // enforce push and pull
		"github.com/NitScm/nit/pkg/blob",    // hold the bytes locally
		"github.com/NitScm/nit/pkg/gitx",    // run git
	}

	const internalPrefix = "github.com/NitScm/nit/internal/"

	for _, target := range edge {
		out, err := exec.Command("go", "list", "-deps", target).Output()
		if err != nil {
			t.Skipf("go list %s: %v", target, err)
		}

		var reached []string

		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if strings.HasPrefix(line, internalPrefix) {
				reached = append(reached, line)
			}
		}

		if len(reached) > 0 {
			t.Errorf("%s now reaches %s.\n\n"+
				"The edge is estimated as a binary that composes packages which already "+
				"exist. That estimate is only true while these packages can be built "+
				"without the rest of the module, and this one can no longer be. Move the "+
				"helper into pkg/, or take the dependency deliberately and update the "+
				"gap — but do not leave it here, because the next person to read the "+
				"roadmap will believe the number in it.",
				target, strings.Join(reached, ", "))
		}
	}
}

// And the edge must not need a database, a socket or a clock to make a
// decision.
//
// The rule that `pkg/` performs no IO is written in CLAUDE.md and is what makes
// the authorization path testable without infrastructure. It is also what makes
// the *edge* possible: a customer-run binary that had to reach a database to
// decide whether a push is allowed would be a control plane, not an edge, and
// the split would buy nothing.
//
// `pkg/nitd` is the documented exception and is not in the list above.
func TestTheEdgePackagesTakeNoInfrastructure(t *testing.T) {
	// Packages whose presence in a dependency tree means IO. `os` and `os/exec`
	// are absent on purpose: pkg/gitx runs git and pkg/blob names files, which
	// is the edge's whole point — it is the side of the line that touches the
	// customer's own disk.
	infrastructure := []string{
		"database/sql",
		"net/http",
	}

	decide := []string{
		"github.com/NitScm/nit/pkg/patch",
		"github.com/NitScm/nit/pkg/policy",
		"github.com/NitScm/nit/pkg/enforce",
	}

	for _, target := range decide {
		out, err := exec.Command("go", "list", "-deps", target).Output()
		if err != nil {
			t.Skipf("go list %s: %v", target, err)
		}

		deps := strings.Split(strings.TrimSpace(string(out)), "\n")

		for _, unwanted := range infrastructure {
			for _, dep := range deps {
				if dep == unwanted {
					t.Errorf("%s now depends on %s.\n\n"+
						"Deciding whether a push is allowed must not require a database or "+
						"a network. An edge that did would be a control plane wearing a "+
						"different name, and the customer-run half of the split would stop "+
						"being runnable while ours is down.",
						target, unwanted)
				}
			}
		}
	}
}
