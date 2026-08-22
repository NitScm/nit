package policy

import "testing"

func TestPatternMatch(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		// Subtree form.
		{"secrets/", "secrets/prod.env", true},
		{"secrets/", "secrets/nested/deep/key.pem", true},
		{"secrets/", "secrets", true},
		{"secrets/", "secretsfile", false},
		{"secrets/", "src/secrets/x", false},

		// Glob form, scattered files.
		{"**/*.env", "src/config/local.env", true},
		{"**/*.env", "a/b/c/d.env", true},
		{"*.env", "top.env", true},
		{"*.env", "src/nested.env", false},

		// Single-segment wildcard does not cross separators.
		{"src/*.go", "src/app.go", true},
		{"src/*.go", "src/nested/app.go", false},
		{"src/**/*.go", "src/nested/app.go", true},

		// Everything.
		{"**", "anything/at/all.txt", true},
		{"**", "top", true},

		// Alternation and character classes.
		{"{docs,site}/**", "docs/readme.md", true},
		{"{docs,site}/**", "src/readme.md", false},

		// Exact literal.
		{".github/workflows/ci.yml", ".github/workflows/ci.yml", true},
		{".github/workflows/ci.yml", ".github/workflows/other.yml", false},
	}

	for _, tc := range cases {
		p := MustParsePattern(tc.pattern)

		if got := p.Match(tc.path); got != tc.want {
			t.Errorf("Pattern(%q).Match(%q) = %v, want %v", tc.pattern, tc.path, got, tc.want)
		}
	}
}

func TestParsePatternRejectsInvalid(t *testing.T) {
	invalid := []string{
		"",
		"/absolute/path",
		`windows\style`,
		"double//slash",
		"../escape",
		"a/./b",
	}

	for _, s := range invalid {
		if _, err := ParsePattern(s); err == nil {
			t.Errorf("ParsePattern(%q) accepted an invalid pattern", s)
		}
	}
}

func TestPatternSpecificity(t *testing.T) {
	more := MustParsePattern("src/internal/api/")
	less := MustParsePattern("src/**")

	if more.Specificity() <= less.Specificity() {
		t.Errorf("expected %q to be more specific than %q", more, less)
	}
}

func TestPatternIsSubtree(t *testing.T) {
	if !MustParsePattern("secrets/").IsSubtree() {
		t.Error("trailing slash should mark a subtree")
	}
	if MustParsePattern("secrets/**").IsSubtree() {
		t.Error("glob form is not a subtree pattern")
	}
}
