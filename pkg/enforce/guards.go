package enforce

import (
	"github.com/NitScm/nit/pkg/patch"
	"github.com/NitScm/nit/pkg/policy"
)

// Guards are the structural protections applied on top of path rules.
//
// Path rules answer "may this subject change this path?". Guards answer the
// question that path rules cannot see: "does this change, whatever path it sits
// at, hand the subject a capability the policy withholds?". A patch that only
// ever touches paths its author may write can still exfiltrate the entire
// repository by editing a CI workflow, and can still expose an unreadable file
// by dropping a symlink next to a readable one.
//
// Every guard is expressed as a requirement for policy.ActionAdmin on the path
// concerned, so guards need no separate configuration language: a bundle grants
// admin on .github/ to the platform team and the guard resolves itself.
type Guards struct {
	// ProtectedPaths require the admin action for any change, on top of the
	// ordinary write requirements.
	ProtectedPaths []policy.Pattern

	// SymlinksRequireAdmin makes creating a symlink, or turning an existing
	// entry into one, require admin on that path.
	SymlinksRequireAdmin bool

	// SubmodulesRequireAdmin makes adding or moving a submodule pointer require
	// admin on that path.
	SubmodulesRequireAdmin bool
}

// DefaultProtectedPaths lists the paths that control how a repository is built,
// checked out and processed. They are protected by default because write access
// to any of them is, in practice, read access to the whole repository: a CI job
// runs with a full checkout and can print anything it wants.
//
// Deployments extend this list; they should not shorten it without deciding
// what replaces it.
var DefaultProtectedPaths = []string{
	// Forge-native CI and automation.
	".github/",
	".gitlab-ci.yml",
	".gitlab/",
	".gitea/",
	".circleci/",
	".drone.yml",
	"azure-pipelines.yml",
	"Jenkinsfile",
	"**/Jenkinsfile",

	// Git mechanisms that run code or rewrite content on checkout.
	".gitattributes",
	"**/.gitattributes",
	".gitmodules",

	// nit's own configuration, when a repository carries any.
	".nit/",
}

// DefaultGuards returns the guard set used when a deployment does not override
// it.
func DefaultGuards() Guards {
	patterns := make([]policy.Pattern, 0, len(DefaultProtectedPaths))
	for _, s := range DefaultProtectedPaths {
		patterns = append(patterns, policy.MustParsePattern(s))
	}

	return Guards{
		ProtectedPaths:         patterns,
		SymlinksRequireAdmin:   true,
		SubmodulesRequireAdmin: true,
	}
}

// requirements returns the extra admin requirements a change triggers.
func (g Guards) requirements(c *patch.Change) []requirement {
	var reqs []requirement

	if g.SymlinksRequireAdmin && introducesKind(c, patch.KindSymlink) {
		reqs = append(reqs, requirement{c.DisplayPath(), policy.ActionAdmin, GuardSymlink})
	}

	if g.SubmodulesRequireAdmin && introducesKind(c, patch.KindSubmodule) {
		reqs = append(reqs, requirement{c.DisplayPath(), policy.ActionAdmin, GuardSubmodule})
	}

	for _, path := range c.Paths() {
		if g.isProtected(path) {
			reqs = append(reqs, requirement{path, policy.ActionAdmin, GuardProtectedPath})
		}
	}

	return reqs
}

func (g Guards) isProtected(path string) bool {
	for _, p := range g.ProtectedPaths {
		if p.Match(path) {
			return true
		}
	}
	return false
}

// introducesKind reports whether the change results in an entry of the given
// kind where there was not already one.
//
// A section that states no mode at all — a pure rename, for instance — cannot
// introduce anything: the mode is whatever upstream already had. Treating an
// unknown mode as suspicious would flag every rename in every patch.
func introducesKind(c *patch.Change, kind patch.EntryKind) bool {
	if c.NewKind != kind {
		return false
	}

	// Moving an existing entry of that kind to a new path is still an
	// introduction at the destination.
	if c.Op == patch.OpRename || c.Op == patch.OpCopy {
		return true
	}

	return c.OldKind != kind
}
