package gitx

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/NitScm/nit/pkg/patch"
)

func requireGit(t *testing.T) *ExecGit {
	t.Helper()

	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}

	return NewExecGit()
}

// initRepo builds a small repository with one commit and returns the repo and
// the commit hash.
func initRepo(t *testing.T, g *ExecGit) (Repo, string) {
	t.Helper()

	ctx := context.Background()
	dir := t.TempDir()

	for _, args := range [][]string{
		{"init", "--initial-branch=main", "."},
		{"config", "user.email", "test@example.com"},
		{"config", "user.name", "Test"},
	} {
		if _, err := g.run(ctx, dir, args...); err != nil {
			t.Fatalf("setup %v: %v", args, err)
		}
	}

	write(t, dir, "src/app.go", "package main\n\nfunc main() {}\n")
	write(t, dir, "secrets/prod.env", "TOKEN=abc\n")

	if _, err := g.run(ctx, dir, "add", "--all"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := g.run(ctx, dir, "commit", "-m", "init"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	repo, err := g.Open(ctx, dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	head, err := repo.ResolveRef(ctx, "HEAD")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}

	return repo, head
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()

	full := filepath.Join(dir, name)

	if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// The output of Diff must be exactly what the patch package can parse: this is
// the seam between the two, and it is where flag drift would bite.
func TestDiffProducesParseablePatch(t *testing.T) {
	g := requireGit(t)
	ctx := context.Background()

	repo, base := initRepo(t, g)

	write(t, repo.Dir(), "src/app.go", "package main\n\nfunc main() { println(1) }\n")
	write(t, repo.Dir(), "src/new.go", "package main\n")

	if _, err := g.run(ctx, repo.Dir(), "add", "--all"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := g.run(ctx, repo.Dir(), "commit", "-m", "change"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	head, err := repo.ResolveRef(ctx, "HEAD")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}

	raw, err := repo.Diff(ctx, base, head)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	set, err := patch.Parse(raw)
	if err != nil {
		t.Fatalf("patch.Parse on gitx output: %v", err)
	}

	if len(set.Changes) != 2 {
		t.Errorf("got %d sections, want 2: %v", len(set.Changes), set.Paths())
	}

	paths, err := repo.ChangedPaths(ctx, base, head)
	if err != nil {
		t.Fatalf("ChangedPaths: %v", err)
	}
	if len(paths) != 2 {
		t.Errorf("ChangedPaths = %v, want 2 entries", paths)
	}
}

// A patch produced by nit, filtered, and applied back must reconstruct exactly
// the intended state — this is the push worker's core loop in miniature.
func TestApplyFilteredPatch(t *testing.T) {
	g := requireGit(t)
	ctx := context.Background()

	source, base := initRepo(t, g)

	write(t, source.Dir(), "src/app.go", "package main\n\nfunc main() { println(1) }\n")
	write(t, source.Dir(), "secrets/prod.env", "TOKEN=leaked\n")

	if _, err := g.run(ctx, source.Dir(), "add", "--all"); err != nil {
		t.Fatalf("add: %v", err)
	}
	if _, err := g.run(ctx, source.Dir(), "commit", "-m", "change"); err != nil {
		t.Fatalf("commit: %v", err)
	}

	head, err := source.ResolveRef(ctx, "HEAD")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}

	raw, err := source.Diff(ctx, base, head)
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}

	set, err := patch.Parse(raw)
	if err != nil {
		t.Fatalf("patch.Parse: %v", err)
	}

	filtered := set.Render(func(c *patch.Change) bool {
		return c.DisplayPath() != "secrets/prod.env"
	})

	// Apply to a fresh clone sitting at the base commit, which is what a worker
	// does.
	target, err := g.Clone(ctx, source.Dir(), filepath.Join(t.TempDir(), "clone"), CloneOptions{})
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if err := target.Checkout(ctx, base); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	if err := target.Apply(ctx, filtered, ApplyOptions{Check: true}); err != nil {
		t.Fatalf("Apply --check: %v", err)
	}
	if err := target.Apply(ctx, filtered, ApplyOptions{ThreeWay: true}); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(target.Dir(), "src", "app.go"))
	if err != nil {
		t.Fatalf("read applied file: %v", err)
	}
	if string(got) != "package main\n\nfunc main() { println(1) }\n" {
		t.Errorf("applied content = %q", got)
	}

	secret, err := os.ReadFile(filepath.Join(target.Dir(), "secrets", "prod.env"))
	if err != nil {
		t.Fatalf("read secret: %v", err)
	}
	if string(secret) != "TOKEN=abc\n" {
		t.Errorf("filtered-out file was modified: %q", secret)
	}

	commit, err := target.CommitAll(ctx, "filtered change", Author{Name: "nit", Email: "nit@example.com"})
	if err != nil {
		t.Fatalf("CommitAll: %v", err)
	}
	if commit == "" {
		t.Error("CommitAll returned no commit hash")
	}
}

// Env has to override an inherited variable, not merely be appended after it.
//
// It is what carries git.ssh_command to a worker, and a setting that silently
// did nothing on a host already exporting GIT_SSH_COMMAND would be worse than
// no setting at all: the wrong key would be offered and the failure would look
// like a permissions problem on the forge.
func TestEnvOverridesTheInheritedEnvironment(t *testing.T) {
	g := requireGit(t)
	ctx := context.Background()

	dir := t.TempDir()
	marker := filepath.Join(dir, "chosen")

	// Two transports that both fail, distinguishable by what they leave behind.
	inherited := writeScript(t, dir, "inherited.sh", "")
	configured := writeScript(t, dir, "configured.sh", marker)

	t.Setenv("GIT_SSH_COMMAND", inherited)
	g.Env = []string{"GIT_SSH_COMMAND=" + configured}

	// The clone must fail: neither script speaks the git protocol. What matters
	// is which one git ran.
	if _, err := g.Clone(ctx, "ssh://git@example.invalid/repo.git",
		filepath.Join(dir, "clone"), CloneOptions{}); err == nil {
		t.Fatal("the clone was expected to fail")
	}

	if _, err := os.Stat(marker); err != nil {
		t.Errorf("git used the inherited GIT_SSH_COMMAND, not the configured one")
	}
}

// writeScript writes an executable that touches marker (when non-empty) and
// fails, standing in for an SSH transport.
func writeScript(t *testing.T, dir, name, marker string) string {
	t.Helper()

	body := "#!/bin/sh\n"
	if marker != "" {
		body += ": > " + marker + "\n"
	}
	body += "exit 1\n"

	path := filepath.Join(dir, name)

	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatalf("write script: %v", err)
	}

	return path
}
