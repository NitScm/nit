package policyloader_test

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/NitScm/nit/internal/policyloader"
	"github.com/NitScm/nit/pkg/policy"
)

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// writeBundle creates a minimal, valid bundle and returns its directory.
func writeBundle(t *testing.T, rule string) string {
	t.Helper()

	dir := t.TempDir()

	write(t, filepath.Join(dir, "users.yaml"), "- id: alice\n  email: alice@example.com\n")
	write(t, filepath.Join(dir, "groups.yaml"), "- id: devs\n  members: [alice]\n")
	write(t, filepath.Join(dir, "repositories.yaml"),
		"- id: backend-api\n  remote: https://example.com/r.git\n  forge: github\n  default_branch: main\n")
	write(t, filepath.Join(dir, "repositories", "backend-api", "rules.yaml"), rule)

	return dir
}

func write(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

const validRule = `- id: devs-read-src
  subject: { type: group, id: devs }
  paths: [src/]
  actions: [read]
  effect: allow
`

func TestNewLoadsBundle(t *testing.T) {
	l, err := policyloader.New(writeBundle(t, validRule), quiet())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if l.Current() == nil {
		t.Fatal("Current returned nil after a successful load")
	}
	if len(l.Current().Repositories()) != 1 {
		t.Error("the bundle was not loaded")
	}
}

// A server with no policy can answer no question correctly, so refusing to
// start is the only safe option.
func TestNewFailsOnBrokenBundle(t *testing.T) {
	dir := writeBundle(t, "- id: broken\n  subject: { type: group, id: nonexistent }\n  paths: [src/]\n  actions: [read]\n  effect: allow\n")

	if _, err := policyloader.New(dir, quiet()); err == nil {
		t.Error("a bundle referencing an unknown group must fail the load")
	}
}

func TestReloadPicksUpChanges(t *testing.T) {
	dir := writeBundle(t, validRule)

	l, err := policyloader.New(dir, quiet())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	before := l.Current().Version()

	var reloaded string
	l.OnReload = func(p *policy.Policy) { reloaded = p.Version() }

	// An unchanged bundle is not a reload.
	if changed, err := l.Reload(); err != nil || changed {
		t.Errorf("Reload on an unchanged bundle: changed=%v err=%v", changed, err)
	}

	write(t, filepath.Join(dir, "repositories", "backend-api", "rules.yaml"), validRule+`
- id: devs-write-src
  subject: { type: group, id: devs }
  paths: [src/]
  actions: [read, write]
  effect: allow
`)

	changed, err := l.Reload()
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !changed {
		t.Fatal("Reload did not report the change")
	}
	if l.Current().Version() == before {
		t.Error("the version did not move")
	}
	if reloaded != l.Current().Version() {
		t.Error("OnReload was not called with the new bundle")
	}
}

// The rule that shapes this package: a bundle that does not compile changes
// nothing. Failing open would grant access nobody authorized; failing closed
// would take an outage on every typo.
func TestFailedReloadKeepsPreviousBundle(t *testing.T) {
	dir := writeBundle(t, validRule)

	l, err := policyloader.New(dir, quiet())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	before := l.Current()

	write(t, filepath.Join(dir, "repositories", "backend-api", "rules.yaml"),
		"- id: broken\n  subject: { type: group, id: nonexistent }\n  paths: [src/]\n  actions: [read]\n  effect: allow\n")

	if _, err := l.Reload(); err == nil {
		t.Fatal("a broken bundle must fail the reload")
	}

	if l.Current() != before {
		t.Error("the broken bundle replaced the working one")
	}
}

func TestStaticSource(t *testing.T) {
	l, err := policyloader.New(writeBundle(t, validRule), quiet())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	static := policyloader.Static{Policy: l.Current()}

	if static.Current() != l.Current() {
		t.Error("Static did not return its policy")
	}

	// A static loader has no directory and must not pretend to reload.
	fixed := policyloader.NewStatic(l.Current())

	if changed, err := fixed.Reload(); err != nil || changed {
		t.Errorf("Reload on a static loader: changed=%v err=%v", changed, err)
	}
}
