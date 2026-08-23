package nitd_test

import (
	"os/exec"
	"strings"
	"testing"
)

// This package takes the only exception to the rule that `pkg/` performs no IO,
// and the exception is safe for exactly one reason: nothing else in the module
// imports it. That makes it a leaf, so no amount of IO in here can reach the
// authorization path, which is what the rule protects.
//
// A reason like that decays the moment somebody imports the package for a
// helper. This test is what stops that being discovered later.
func TestNothingInTheModuleImportsThisPackage(t *testing.T) {
	const self = "github.com/NitScm/nit/pkg/nitd"

	// The binaries are meant to. They are the callers this package exists for,
	// and their importing it is what keeps the façade from drifting.
	allowed := map[string]bool{
		"github.com/NitScm/nit/cmd/nitd":       true,
		"github.com/NitScm/nit/cmd/nit-worker": true,
	}

	out, err := exec.Command("go", "list", "-deps=false",
		"-f", "{{.ImportPath}} {{join .Imports \",\"}} {{join .TestImports \",\"}}",
		"github.com/NitScm/nit/...").Output()
	if err != nil {
		t.Skipf("go list: %v", err)
	}

	for line := range strings.SplitSeq(strings.TrimSpace(string(out)), "\n") {
		fields := strings.SplitN(line, " ", 2)
		if len(fields) != 2 {
			continue
		}

		pkg := fields[0]
		if pkg == self || allowed[pkg] {
			continue
		}

		for imported := range strings.FieldsSeq(fields[1]) {
			for one := range strings.SplitSeq(imported, ",") {
				if one == self {
					t.Errorf("%s imports %s\n\n"+
						"That package performs IO, and it is allowed to only because it is a "+
						"leaf: nothing else importing it is what keeps the IO out of the "+
						"authorization path. Move what you need, or move this package's job "+
						"into internal/.", pkg, self)
				}
			}
		}
	}
}
