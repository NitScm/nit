package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/NitScm/nit/internal/client"
	"github.com/NitScm/nit/internal/flow"
	"github.com/NitScm/nit/internal/workspace"
	"github.com/NitScm/nit/pkg/gitx"
	"github.com/NitScm/nit/pkg/protocol"
)

// devMachine drives the real client code against the running stack.
//
// Nothing here is a test double: the runner is the one cmd/nit uses, the
// workspace is a real checkout on disk, and the server and worker are the real
// ones. What this exercises that the layer tests cannot is the seams — a field
// the server sets and the client reads under another name, a sync token minted
// with one meaning and consumed with another.
type devMachine struct {
	t *testing.T

	runner *flow.Runner
	dir    string
	ws     *workspace.Workspace
}

// newDevMachine wires a client for the stack, with a worker draining the queue
// in the background so the flows see real asynchronous behaviour rather than a
// queue somebody flushed by hand.
func newDevMachine(t *testing.T, s *stack) *devMachine {
	t.Helper()

	c := client.New(s.http.URL, s.token)

	m := &devMachine{
		t: t,
		runner: &flow.Runner{
			Client: c,
			Git:    gitx.NewExecGit(),
		},
		dir: filepath.Join(t.TempDir(), "checkout"),
	}

	s.startWorker(t)

	return m
}

// startWorker drains the queue continuously until the test ends.
func (s *stack) startWorker(t *testing.T) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())

	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()

		for {
			if ctx.Err() != nil {
				return
			}

			handle, err := s.queue.Claim(ctx, "worker-1")
			if err != nil {
				select {
				case <-ctx.Done():
					return
				default:
				}
				continue
			}

			result, err := s.worker.Handle(handle.Context(), handle.Task)
			if err != nil {
				_, _ = handle.Fail(context.Background(), asProtocolError(err))
				continue
			}

			_ = handle.Complete(context.Background(), result)
		}
	}()

	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
}

func (m *devMachine) clone(s *stack) *flow.CloneResult {
	m.t.Helper()

	result, err := m.runner.Clone(context.Background(), flow.CloneOptions{
		Server:     s.http.URL,
		Repository: "backend-api",
		Branch:     "main",
		Directory:  m.dir,
		Label:      "test-laptop",
	})
	if err != nil {
		m.t.Fatalf("clone: %v", err)
	}

	m.ws, err = workspace.Open(context.Background(), gitx.NewExecGit(), m.dir)
	if err != nil {
		m.t.Fatalf("open workspace: %v", err)
	}

	return result
}

// commit records local work, as a developer would before pushing.
func (m *devMachine) commit(message string) {
	m.t.Helper()

	git(m.t, m.dir, "add", "--all")
	git(m.t, m.dir, "commit", "-m", message)

	reopened, err := workspace.Open(context.Background(), gitx.NewExecGit(), m.dir)
	if err != nil {
		m.t.Fatalf("reopen workspace: %v", err)
	}
	m.ws = reopened
}

func (m *devMachine) push(message string) *flow.PushResult {
	m.t.Helper()

	result, err := m.runner.Push(context.Background(), m.ws, flow.PushOptions{Message: message})
	if err != nil {
		m.t.Fatalf("push: %v", err)
	}

	m.reload()

	return result
}

func (m *devMachine) pull() protocol.PullReport {
	m.t.Helper()

	report, err := m.runner.Pull(context.Background(), m.ws)
	if err != nil {
		m.t.Fatalf("pull: %v", err)
	}

	m.reload()

	return report
}

func (m *devMachine) reload() {
	m.t.Helper()

	reopened, err := workspace.Open(context.Background(), gitx.NewExecGit(), m.dir)
	if err != nil {
		m.t.Fatalf("reopen workspace: %v", err)
	}
	m.ws = reopened
}

func (m *devMachine) exists(path string) bool {
	m.t.Helper()

	_, err := os.Stat(filepath.Join(m.dir, path))
	return err == nil
}

func (m *devMachine) read(path string) string {
	m.t.Helper()

	content, err := os.ReadFile(filepath.Join(m.dir, path))
	if err != nil {
		m.t.Fatalf("read %s: %v", path, err)
	}

	return string(content)
}

// ---------------------------------------------------------------------------

// The product, from the outside: clone, work, push, pull — with a confidential
// subtree that never reaches the developer's disk.
func TestDeveloperLifecycle(t *testing.T) {
	s := newStack(t)
	m := newDevMachine(t, s)

	// --- clone ---

	result := m.clone(s)

	if !m.exists("src/app.go") || !m.exists("docs/readme.md") {
		t.Fatal("the clone did not deliver the readable files")
	}
	if m.exists("secrets/prod.env") {
		t.Fatal("the confidential file reached the developer's disk")
	}
	if result.Report.FilesWithheld != 1 {
		t.Errorf("FilesWithheld = %d, want 1", result.Report.FilesWithheld)
	}
	if m.ws.State.SyncToken == "" || m.ws.State.LocalBase == "" {
		t.Fatal("the workspace recorded no sync point")
	}

	// The synchronization commit carries the trailers that make the state
	// recoverable from git history alone.
	message := git(t, m.dir, "log", "-1", "--format=%B")

	if !strings.Contains(message, "Nit-Upstream-Commit: "+s.upstreamHead()) {
		t.Errorf("the sync commit does not name the upstream commit:\n%s", message)
	}
	if !strings.Contains(message, "Nit-Workspace: "+m.ws.State.Workspace) {
		t.Errorf("the sync commit does not name the workspace:\n%s", message)
	}

	// --- work and push ---

	write(t, m.dir, "src/app.go", "package main\n\nfunc main() { println(1) }\n")
	m.commit("local work")

	push := m.push("add a print")

	if push.UpstreamCommit != s.upstreamHead() {
		t.Errorf("reported %s, upstream is at %s", push.UpstreamCommit, s.upstreamHead())
	}
	if push.NeedsPull {
		t.Error("an unimpeded push should not require a pull")
	}
	if got := s.upstreamFile("src/app.go"); !strings.Contains(got, "println(1)") {
		t.Errorf("upstream content = %q", got)
	}

	// The workspace stayed faithful, so its base moved with the push and a
	// second push needs no round trip through pull.
	if m.ws.State.LocalBase != git(t, m.dir, "rev-parse", "HEAD") {
		t.Error("the local base did not advance after a faithful push")
	}

	// --- somebody else changes both a readable and a confidential file ---

	s.commitUpstream("docs/readme.md", "updated elsewhere\n", "docs update")
	s.commitUpstream("secrets/prod.env", "TOKEN=rotated\n", "rotate the secret")

	report := m.pull()

	if got := m.read("docs/readme.md"); got != "updated elsewhere\n" {
		t.Errorf("the readable change was not applied: %q", got)
	}
	if m.exists("secrets/prod.env") {
		t.Error("the confidential file appeared after a pull")
	}
	if report.FilesDelivered != 1 || report.FilesWithheld != 1 {
		t.Errorf("report = %+v", report)
	}

	// --- and the developer can push again from the new state ---

	write(t, m.dir, "docs/readme.md", "updated elsewhere, then by me\n")
	m.commit("more local work")

	second := m.push("update the docs")

	if second.UpstreamCommit != s.upstreamHead() {
		t.Error("the second push did not land")
	}
	if got := s.upstreamFile("docs/readme.md"); got != "updated elsewhere, then by me" {
		t.Errorf("upstream content = %q", got)
	}
}

// A push touching a confidential path is refused, and the developer is told
// which rule refused it and what to do.
func TestPushRefusedWithActionableMessage(t *testing.T) {
	s := newStack(t)
	m := newDevMachine(t, s)

	m.clone(s)

	// The developer cannot see secrets/, but nothing stops them creating a file
	// at that path.
	write(t, m.dir, "secrets/prod.env", "TOKEN=mine\n")
	m.commit("try to write a secret")

	before := s.upstreamHead()

	_, err := m.runner.Push(context.Background(), m.ws, flow.PushOptions{Message: "should not land"})
	if err == nil {
		t.Fatal("the push was not refused")
	}

	if !client.IsCode(err, protocol.CodeUnauthorizedPaths) {
		t.Fatalf("error = %v, want an unauthorized_paths failure", err)
	}

	var apiErr *client.Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("error is not a client error: %v", err)
	}

	var named bool
	for _, denial := range apiErr.Denials {
		if denial.Path == "secrets/prod.env" {
			named = true

			if denial.RuleID == "" {
				t.Error("the denial names no rule")
			}
			if denial.Description == "" {
				t.Error("the denial carries no message for the developer")
			}
		}
	}
	if !named {
		t.Errorf("the denials do not name the offending path: %+v", apiErr.Denials)
	}

	if s.upstreamHead() != before {
		t.Error("a refused push changed the forge")
	}

	// The workspace is untouched: the developer fixes the commit and retries.
	if m.ws.State.LocalBase == git(t, m.dir, "rev-parse", "HEAD") {
		t.Error("a refused push advanced the local base")
	}
}

// --check answers the question without submitting anything.
func TestPushCheckQueuesNothing(t *testing.T) {
	s := newStack(t)
	m := newDevMachine(t, s)

	m.clone(s)

	write(t, m.dir, "src/app.go", "package main\n\nfunc main() { println(2) }\n")
	m.commit("local work")

	before := s.upstreamHead()

	result, err := m.runner.Push(context.Background(), m.ws, flow.PushOptions{Check: true})
	if err != nil {
		t.Fatalf("check: %v", err)
	}

	if result.Report.FilesAccepted != 1 {
		t.Errorf("report = %+v", result.Report)
	}
	if result.UpstreamCommit != "" {
		t.Error("a check reported an upstream commit")
	}
	if s.upstreamHead() != before {
		t.Error("a check changed the forge")
	}
}

// A push landing on a branch that moved leaves the workspace unfaithful, and
// the developer is told so rather than left to discover it later.
func TestPushOntoMovedBranchAsksForAPull(t *testing.T) {
	s := newStack(t)
	m := newDevMachine(t, s)

	m.clone(s)

	write(t, m.dir, "src/app.go", "package main\n\nfunc main() { println(3) }\n")
	m.commit("local work")

	// Someone else pushes an unrelated file first.
	s.commitUpstream("docs/other.md", "from someone else\n", "concurrent change")

	result := m.push("add a print")

	if result.UpstreamCommit == "" {
		t.Fatal("the push did not land")
	}
	if !result.NeedsPull {
		t.Error("the workspace is missing a commit but was not told to pull")
	}

	// Both changes survive upstream.
	if got := s.upstreamFile("src/app.go"); !strings.Contains(got, "println(3)") {
		t.Errorf("our change is missing: %q", got)
	}
	if got := s.upstreamFile("docs/other.md"); got != "from someone else" {
		t.Errorf("the concurrent change was lost: %q", got)
	}

	// The local base did not move, so the next push would be refused as stale —
	// and the pull that fixes it works.
	if m.ws.State.LocalBase == git(t, m.dir, "rev-parse", "HEAD") {
		t.Error("the local base advanced although the workspace is no longer faithful")
	}

	m.pull()

	if !m.exists("docs/other.md") {
		t.Error("the pull did not deliver the colleague's change")
	}
}

// A clone refuses to overwrite an existing directory, and cleans up after
// itself when the pull it depends on fails.
func TestCloneIntoNonEmptyDirectory(t *testing.T) {
	s := newStack(t)
	m := newDevMachine(t, s)

	if err := os.MkdirAll(m.dir, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	write(t, m.dir, "existing.txt", "do not clobber me\n")

	_, err := m.runner.Clone(context.Background(), flow.CloneOptions{
		Server:     s.http.URL,
		Repository: "backend-api",
		Branch:     "main",
		Directory:  m.dir,
	})
	if err == nil {
		t.Fatal("clone overwrote a non-empty directory")
	}

	if content := m.read("existing.txt"); content != "do not clobber me\n" {
		t.Errorf("the existing file was touched: %q", content)
	}
}

// A pull must not silently merge into uncommitted work.
func TestPullRefusesADirtyWorkspace(t *testing.T) {
	s := newStack(t)
	m := newDevMachine(t, s)

	m.clone(s)

	write(t, m.dir, "src/app.go", "uncommitted work\n")

	s.commitUpstream("docs/readme.md", "changed\n", "docs update")

	_, err := m.runner.Pull(context.Background(), m.ws)
	if err == nil {
		t.Fatal("the pull ran against a dirty working tree")
	}
	if !strings.Contains(err.Error(), "uncommitted") {
		t.Errorf("error = %v; it must say what is wrong", err)
	}

	if got := m.read("src/app.go"); got != "uncommitted work\n" {
		t.Errorf("the pull touched uncommitted work: %q", got)
	}
}

func TestPushWithNothingToSend(t *testing.T) {
	s := newStack(t)
	m := newDevMachine(t, s)

	m.clone(s)

	_, err := m.runner.Push(context.Background(), m.ws, flow.PushOptions{Message: "nothing"})
	if err == nil || !strings.Contains(err.Error(), "nothing to push") {
		t.Errorf("error = %v, want a clear \"nothing to push\"", err)
	}
}

// Two workspaces of the same developer stay independent: each has its own sync
// point, which is what makes a laptop and a desktop work.
func TestTwoWorkspacesAreIndependent(t *testing.T) {
	s := newStack(t)

	laptop := newDevMachine(t, s)
	laptop.clone(s)

	desktop := &devMachine{
		t:      t,
		runner: laptop.runner,
		dir:    filepath.Join(t.TempDir(), "desktop"),
	}
	desktop.clone(s)

	if laptop.ws.State.Workspace == desktop.ws.State.Workspace {
		t.Fatal("the two checkouts share a workspace id")
	}

	// The laptop pushes; the desktop is now behind and must pull.
	write(t, laptop.dir, "src/app.go", "package main\n\nfunc main() { println(4) }\n")
	laptop.commit("laptop work")
	laptop.push("from the laptop")

	if desktop.exists("src/app.go") {
		if content := desktop.read("src/app.go"); strings.Contains(content, "println(4)") {
			t.Error("the desktop already has the laptop's change without pulling")
		}
	}

	desktop.pull()

	if content := desktop.read("src/app.go"); !strings.Contains(content, "println(4)") {
		t.Errorf("the desktop did not receive the change: %q", content)
	}
}
