package policy

import (
	"fmt"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
)

// Pattern matches repository paths (or refs). Two shapes are supported, because
// the two ways people describe confidential areas are genuinely different:
//
//	secrets/          subtree — the directory itself and everything under it
//	**/*.env          glob    — files scattered across the tree
//
// A trailing slash is the explicit, unambiguous marker for "subtree". Nothing
// is inferred from the presence or absence of a dot in the last segment: a rule
// that silently changes meaning because a directory was named "v1.0" is a
// security incident waiting to happen.
//
// Glob syntax is doublestar's: "*" matches within one segment, "**" matches
// across segments, "?" matches one character, "[...]" a character class, and
// "{a,b}" an alternation.
type Pattern struct {
	raw  string
	glob string

	// dir is the directory a subtree pattern is rooted at, empty otherwise. It
	// is matched exactly, so that "secrets/" also covers the entry "secrets"
	// itself (a submodule or a symlink placed at that exact path).
	dir string
}

// MatchAll is the pattern covering every path.
var MatchAll = Pattern{raw: "**", glob: "**"}

// ParsePattern validates and compiles a pattern.
func ParsePattern(s string) (Pattern, error) {
	if s == "" {
		return Pattern{}, fmt.Errorf("policy: empty pattern")
	}
	if strings.HasPrefix(s, "/") {
		return Pattern{}, fmt.Errorf("policy: pattern %q must be repository-relative (no leading %q)", s, "/")
	}
	if strings.Contains(s, `\`) {
		return Pattern{}, fmt.Errorf("policy: pattern %q must use %q as separator", s, "/")
	}
	if strings.Contains(s, "//") {
		return Pattern{}, fmt.Errorf("policy: pattern %q contains an empty path segment", s)
	}
	for seg := range strings.SplitSeq(s, "/") {
		if seg == ".." || seg == "." {
			return Pattern{}, fmt.Errorf("policy: pattern %q must not contain %q or %q segments", s, ".", "..")
		}
	}

	p := Pattern{raw: s}

	if strings.HasSuffix(s, "/") {
		p.dir = strings.TrimSuffix(s, "/")
		p.glob = p.dir + "/**"
	} else {
		p.glob = s
	}

	if !doublestar.ValidatePattern(p.glob) {
		return Pattern{}, fmt.Errorf("policy: pattern %q is not a valid glob", s)
	}
	if p.dir != "" && !doublestar.ValidatePattern(p.dir) {
		return Pattern{}, fmt.Errorf("policy: pattern %q is not a valid glob", s)
	}

	return p, nil
}

// MustParsePattern is ParsePattern for constants and tests; it panics on an
// invalid pattern.
func MustParsePattern(s string) Pattern {
	p, err := ParsePattern(s)
	if err != nil {
		panic(err)
	}
	return p
}

// Match reports whether the pattern covers path. Paths are expected to be
// repository-relative and slash-separated, exactly as they appear in a patch.
func (p Pattern) Match(path string) bool {
	if p.glob == "" || path == "" {
		return false
	}

	if p.dir != "" {
		if ok, err := doublestar.Match(p.dir, path); err == nil && ok {
			return true
		}
	}

	ok, err := doublestar.Match(p.glob, path)
	return err == nil && ok
}

// IsSubtree reports whether the pattern was written as a subtree.
func (p Pattern) IsSubtree() bool { return p.dir != "" }

// String returns the pattern as it was written.
func (p Pattern) String() string { return p.raw }

// Specificity is the length of the literal prefix before the first wildcard.
// It carries no semantics — deny always wins regardless — and exists only to
// sort explanations so that the most precise rule is reported first.
func (p Pattern) Specificity() int {
	if i := strings.IndexAny(p.glob, "*?[{"); i >= 0 {
		return i
	}
	return len(p.glob)
}
