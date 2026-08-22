package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NitScm/nit/pkg/policy"
)

func exampleBundle() string {
	return filepath.Join("..", "..", "..", "configs", "policy", "example")
}

func loadExample(t *testing.T) *policy.Policy {
	t.Helper()

	p, err := Load(exampleBundle())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return p
}

// The shipped example bundle must always compile: it is both documentation and
// the reference every deployment starts from.
func TestLoadExampleBundle(t *testing.T) {
	p := loadExample(t)

	if len(p.Repositories()) != 1 {
		t.Errorf("got %d repositories, want 1", len(p.Repositories()))
	}
	if !strings.HasPrefix(p.Version(), "sha256:") {
		t.Errorf("Version = %q, want a content hash", p.Version())
	}
	if _, ok := p.User("alice"); !ok {
		t.Error("alice missing from the bundle")
	}
}

func TestExampleBundleBehaviour(t *testing.T) {
	p := loadExample(t)

	cases := []struct {
		name    string
		user    policy.UserID
		ref     string
		path    string
		action  policy.Action
		allowed bool
	}{
		{"backend writes server code", "alice", "refs/heads/feature/x", "src/server/api.go", policy.ActionWrite, true},
		{"frontend cannot write server code", "bob", "refs/heads/feature/x", "src/server/api.go", policy.ActionWrite, false},
		{"frontend reads server code", "bob", "refs/heads/feature/x", "src/server/api.go", policy.ActionRead, true},
		{"shared code is writable by both", "bob", "refs/heads/feature/x", "src/shared/util.go", policy.ActionWrite, true},

		// Confidential subtree, and the exemption for the platform team.
		{"secrets hidden from frontend", "bob", "refs/heads/feature/x", "secrets/prod.key", policy.ActionRead, false},
		{"secrets readable by platform", "alice", "refs/heads/feature/x", "secrets/prod.key", policy.ActionRead, true},

		// Scattered credentials have no exemption at all.
		{"credentials denied even to platform", "alice", "refs/heads/feature/x", "src/ui/local.env", policy.ActionRead, false},

		// Branch protection, and its exemption.
		{"no direct push to main", "bob", "refs/heads/main", "src/ui/app.ts", policy.ActionWrite, false},
		{"platform may push to main", "alice", "refs/heads/main", "src/server/api.go", policy.ActionWrite, true},

		// Interns inherit backend grants through group inclusion, but are
		// branch-scoped.
		{"intern writes on a feature branch", "carol", "refs/heads/feature/x", "src/server/api.go", policy.ActionWrite, true},
		{"intern blocked on release branches", "carol", "refs/heads/release/1.2", "src/server/api.go", policy.ActionWrite, false},

		// CI is admin-only.
		{"backend cannot admin CI", "bob", "refs/heads/feature/x", ".github/workflows/ci.yml", policy.ActionAdmin, false},
		{"platform admins CI", "alice", "refs/heads/feature/x", ".github/workflows/ci.yml", policy.ActionAdmin, true},

		// A disabled account is denied regardless of the rules.
		{"disabled user denied", "dave", "refs/heads/feature/x", "docs/readme.md", policy.ActionRead, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subject, err := p.Subject(tc.user)
			if err != nil {
				t.Fatalf("Subject: %v", err)
			}

			d := p.Evaluate(policy.Request{
				Repo:    "backend-api",
				Ref:     tc.ref,
				Subject: subject,
				Path:    tc.path,
				Action:  tc.action,
			})

			if d.Allowed != tc.allowed {
				t.Errorf("Allowed = %v, want %v (%s)", d.Allowed, tc.allowed, d)
			}
		})
	}
}

// The version must change when any byte of the bundle changes, otherwise an
// audit record cannot identify the rules that produced a past decision.
func TestVersionTracksContent(t *testing.T) {
	before := loadExample(t).Version()

	dir := t.TempDir()
	copyTree(t, exampleBundle(), dir)

	extra := filepath.Join(dir, "repositories", "backend-api", "rules.yaml")

	content, err := os.ReadFile(extra)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if err := os.WriteFile(extra, append(content, "\n# a comment\n"...), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	after, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if after.Version() == before {
		t.Error("version did not change after editing the bundle")
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	copyTree(t, exampleBundle(), dir)

	path := filepath.Join(dir, "users.yaml")

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// "email" misspelled: silently ignoring it would drop the address.
	if err := os.WriteFile(path, append(content, "\n- id: eve\n  emial: eve@example.com\n"...), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := Load(dir); err == nil {
		t.Error("a misspelled field must fail the load, not be ignored")
	}
}

func copyTree(t *testing.T, src, dst string) {
	t.Helper()

	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}

		target := filepath.Join(dst, rel)

		if info.IsDir() {
			return os.MkdirAll(target, 0o750)
		}

		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, content, 0o600)
	})
	if err != nil {
		t.Fatalf("copy tree: %v", err)
	}
}
